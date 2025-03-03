package client

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/caarlos0/log"
	"github.com/google/go-github/v48/github"
	"github.com/goreleaser/goreleaser/internal/artifact"
	"github.com/goreleaser/goreleaser/internal/tmpl"
	"github.com/goreleaser/goreleaser/pkg/config"
	"github.com/goreleaser/goreleaser/pkg/context"
	"golang.org/x/oauth2"
)

const DefaultGitHubDownloadURL = "https://github.com"

type githubClient struct {
	client *github.Client
}

// NewGitHub returns a github client implementation.
func NewGitHub(ctx *context.Context, token string) (GitHubClient, error) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)

	httpClient := oauth2.NewClient(ctx, ts)
	base := httpClient.Transport.(*oauth2.Transport).Base
	if base == nil || reflect.ValueOf(base).IsNil() {
		base = http.DefaultTransport
	}
	// nolint: gosec
	base.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: ctx.Config.GitHubURLs.SkipTLSVerify,
	}
	base.(*http.Transport).Proxy = http.ProxyFromEnvironment
	httpClient.Transport.(*oauth2.Transport).Base = base

	client := github.NewClient(httpClient)
	err := overrideGitHubClientAPI(ctx, client)
	if err != nil {
		return &githubClient{}, err
	}

	return &githubClient{client: client}, nil
}

func (c *githubClient) GenerateReleaseNotes(ctx *context.Context, repo Repo, prev, current string) (string, error) {
	notes, _, err := c.client.Repositories.GenerateReleaseNotes(ctx, repo.Owner, repo.Name, &github.GenerateNotesOptions{
		TagName:         current,
		PreviousTagName: github.String(prev),
	})
	if err != nil {
		return "", err
	}
	return notes.Body, err
}

func (c *githubClient) Changelog(ctx *context.Context, repo Repo, prev, current string) (string, error) {
	var log []string
	opts := &github.ListOptions{PerPage: 100}

	for {
		result, resp, err := c.client.Repositories.CompareCommits(ctx, repo.Owner, repo.Name, prev, current, opts)
		if err != nil {
			return "", err
		}
		for _, commit := range result.Commits {
			log = append(log, fmt.Sprintf(
				"%s: %s (@%s)",
				commit.GetSHA(),
				strings.Split(commit.Commit.GetMessage(), "\n")[0],
				commit.GetAuthor().GetLogin(),
			))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return strings.Join(log, "\n"), nil
}

// GetDefaultBranch returns the default branch of a github repo
func (c *githubClient) GetDefaultBranch(ctx *context.Context, repo Repo) (string, error) {
	p, res, err := c.client.Repositories.Get(ctx, repo.Owner, repo.Name)
	if err != nil {
		log.WithFields(log.Fields{
			"projectID":  repo.String(),
			"statusCode": res.StatusCode,
			"err":        err.Error(),
		}).Warn("error checking for default branch")
		return "", err
	}
	return p.GetDefaultBranch(), nil
}

// CloseMilestone closes a given milestone.
func (c *githubClient) CloseMilestone(ctx *context.Context, repo Repo, title string) error {
	milestone, err := c.getMilestoneByTitle(ctx, repo, title)
	if err != nil {
		return err
	}

	if milestone == nil {
		return ErrNoMilestoneFound{Title: title}
	}

	closedState := "closed"
	milestone.State = &closedState

	_, _, err = c.client.Issues.EditMilestone(
		ctx,
		repo.Owner,
		repo.Name,
		*milestone.Number,
		milestone,
	)

	return err
}

func (c *githubClient) CreateFile(
	ctx *context.Context,
	commitAuthor config.CommitAuthor,
	repo Repo,
	content []byte,
	path,
	message string,
) error {
	var branch string
	var err error
	if repo.Branch != "" {
		branch = repo.Branch
	} else {
		branch, err = c.GetDefaultBranch(ctx, repo)
		if err != nil {
			// Fall back to sdk default
			log.WithFields(log.Fields{
				"fileName":        path,
				"projectID":       repo.String(),
				"requestedBranch": branch,
				"err":             err.Error(),
			}).Warn("error checking for default branch, using master")
		}
	}
	options := &github.RepositoryContentFileOptions{
		Committer: &github.CommitAuthor{
			Name:  github.String(commitAuthor.Name),
			Email: github.String(commitAuthor.Email),
		},
		Content: content,
		Message: github.String(message),
	}

	// Set the branch if we got it above...otherwise, just default to
	// whatever the SDK does auto-magically
	if branch != "" {
		options.Branch = &branch
	}

	file, _, res, err := c.client.Repositories.GetContents(
		ctx,
		repo.Owner,
		repo.Name,
		path,
		&github.RepositoryContentGetOptions{},
	)
	if err != nil && (res == nil || res.StatusCode != 404) {
		return err
	}

	if res.StatusCode == 404 {
		_, _, err = c.client.Repositories.CreateFile(
			ctx,
			repo.Owner,
			repo.Name,
			path,
			options,
		)
		return err
	}
	options.SHA = file.SHA
	_, _, err = c.client.Repositories.UpdateFile(
		ctx,
		repo.Owner,
		repo.Name,
		path,
		options,
	)
	return err
}

func (c *githubClient) CreateRelease(ctx *context.Context, body string) (string, error) {
	title, err := tmpl.New(ctx).Apply(ctx.Config.Release.NameTemplate)
	if err != nil {
		return "", err
	}

	if ctx.Config.Release.Draft && ctx.Config.Release.ReplaceExistingDraft {
		if err := c.deleteExistingDraftRelease(ctx, title); err != nil {
			return "", err
		}
	}

	// Truncate the release notes if it's too long (github doesn't allow more than 125000 characters)
	body = truncateReleaseBody(body)

	data := &github.RepositoryRelease{
		Name:       github.String(title),
		TagName:    github.String(ctx.Git.CurrentTag),
		Body:       github.String(body),
		Draft:      github.Bool(ctx.Config.Release.Draft),
		Prerelease: github.Bool(ctx.PreRelease),
	}

	if ctx.Config.Release.DiscussionCategoryName != "" {
		data.DiscussionCategoryName = github.String(ctx.Config.Release.DiscussionCategoryName)
	}

	if target := ctx.Config.Release.TargetCommitish; target != "" {
		target, err := tmpl.New(ctx).Apply(target)
		if err != nil {
			return "", err
		}
		if target != "" {
			data.TargetCommitish = github.String(target)
		}
	}

	release, err := c.createOrUpdateRelease(ctx, data, body)
	if err != nil {
		return "", fmt.Errorf("could not release: %w", err)
	}

	return strconv.FormatInt(release.GetID(), 10), nil
}

func (c *githubClient) createOrUpdateRelease(ctx *context.Context, data *github.RepositoryRelease, body string) (*github.RepositoryRelease, error) {
	release, _, err := c.client.Repositories.GetReleaseByTag(
		ctx,
		ctx.Config.Release.GitHub.Owner,
		ctx.Config.Release.GitHub.Name,
		data.GetTagName(),
	)
	if err != nil {
		release, resp, err := c.client.Repositories.CreateRelease(
			ctx,
			ctx.Config.Release.GitHub.Owner,
			ctx.Config.Release.GitHub.Name,
			data,
		)
		if err == nil {
			log.WithFields(log.Fields{
				"name":       data.GetName(),
				"release-id": release.GetID(),
				"request-id": resp.Header.Get("X-GitHub-Request-Id"),
			}).Info("release created")
		}
		return release, err
	}

	data.Body = github.String(getReleaseNotes(release.GetBody(), body, ctx.Config.Release.ReleaseNotesMode))
	return c.updateRelease(ctx, release.GetID(), data)
}

func (c *githubClient) updateRelease(ctx *context.Context, id int64, data *github.RepositoryRelease) (*github.RepositoryRelease, error) {
	release, resp, err := c.client.Repositories.EditRelease(
		ctx,
		ctx.Config.Release.GitHub.Owner,
		ctx.Config.Release.GitHub.Name,
		id,
		data,
	)
	if err == nil {
		log.WithFields(log.Fields{
			"name":       data.GetName(),
			"release-id": release.GetID(),
			"request-id": resp.Header.Get("X-GitHub-Request-Id"),
		}).Info("release updated")
	}
	return release, err
}

func (c *githubClient) ReleaseURLTemplate(ctx *context.Context) (string, error) {
	downloadURL, err := tmpl.New(ctx).Apply(ctx.Config.GitHubURLs.Download)
	if err != nil {
		return "", fmt.Errorf("templating GitHub download URL: %w", err)
	}

	return fmt.Sprintf(
		"%s/%s/%s/releases/download/{{ .Tag }}/{{ .ArtifactName }}",
		downloadURL,
		ctx.Config.Release.GitHub.Owner,
		ctx.Config.Release.GitHub.Name,
	), nil
}

func (c *githubClient) Upload(
	ctx *context.Context,
	releaseID string,
	artifact *artifact.Artifact,
	file *os.File,
) error {
	githubReleaseID, err := strconv.ParseInt(releaseID, 10, 64)
	if err != nil {
		return err
	}
	_, resp, err := c.client.Repositories.UploadReleaseAsset(
		ctx,
		ctx.Config.Release.GitHub.Owner,
		ctx.Config.Release.GitHub.Name,
		githubReleaseID,
		&github.UploadOptions{
			Name: artifact.Name,
		},
		file,
	)
	if err != nil {
		requestID := ""
		if resp != nil {
			requestID = resp.Header.Get("X-GitHub-Request-Id")
		}
		log.WithFields(log.Fields{
			"name":       artifact.Name,
			"release-id": releaseID,
			"request-id": requestID,
		}).Warn("upload failed")
	}
	if err == nil {
		return nil
	}
	if resp != nil && resp.StatusCode == 422 {
		return err
	}
	return RetriableError{err}
}

// getMilestoneByTitle returns a milestone by title.
func (c *githubClient) getMilestoneByTitle(ctx *context.Context, repo Repo, title string) (*github.Milestone, error) {
	// The GitHub API/SDK does not provide lookup by title functionality currently.
	opts := &github.MilestoneListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		milestones, resp, err := c.client.Issues.ListMilestones(
			ctx,
			repo.Owner,
			repo.Name,
			opts,
		)
		if err != nil {
			return nil, err
		}

		for _, m := range milestones {
			if m != nil && m.Title != nil && *m.Title == title {
				return m, nil
			}
		}

		if resp.NextPage == 0 {
			break
		}

		opts.Page = resp.NextPage
	}

	return nil, nil
}

func overrideGitHubClientAPI(ctx *context.Context, client *github.Client) error {
	if ctx.Config.GitHubURLs.API == "" {
		return nil
	}

	apiURL, err := tmpl.New(ctx).Apply(ctx.Config.GitHubURLs.API)
	if err != nil {
		return fmt.Errorf("templating GitHub API URL: %w", err)
	}
	api, err := url.Parse(apiURL)
	if err != nil {
		return err
	}

	uploadURL, err := tmpl.New(ctx).Apply(ctx.Config.GitHubURLs.Upload)
	if err != nil {
		return fmt.Errorf("templating GitHub upload URL: %w", err)
	}
	upload, err := url.Parse(uploadURL)
	if err != nil {
		return err
	}

	client.BaseURL = api
	client.UploadURL = upload

	return nil
}

func (c *githubClient) deleteExistingDraftRelease(ctx *context.Context, name string) error {
	opt := github.ListOptions{PerPage: 50}
	for {
		releases, resp, err := c.client.Repositories.ListReleases(
			ctx,
			ctx.Config.Release.GitHub.Owner,
			ctx.Config.Release.GitHub.Name,
			&opt,
		)
		if err != nil {
			return fmt.Errorf("could not delete existing drafts: %w", err)
		}
		for _, r := range releases {
			if r.GetDraft() && r.GetName() == name {
				if _, err := c.client.Repositories.DeleteRelease(
					ctx,
					ctx.Config.Release.GitHub.Owner,
					ctx.Config.Release.GitHub.Name,
					r.GetID(),
				); err != nil {
					return fmt.Errorf("could not delete previous draft release: %w", err)
				}

				log.WithFields(log.Fields{
					"commit": r.GetTargetCommitish(),
					"tag":    r.GetTagName(),
					"name":   r.GetName(),
				}).Info("deleted previous draft release")

				// in theory, there should be only 1 release matching, so we can just return
				return nil
			}
		}
		if resp.NextPage == 0 {
			return nil
		}
		opt.Page = resp.NextPage
	}
}
