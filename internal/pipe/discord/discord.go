package discord

import (
	"fmt"
	"strconv"

	"github.com/caarlos0/env/v6"
	"github.com/caarlos0/log"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/webhook"
	"github.com/disgoorg/snowflake/v2"
	"github.com/goreleaser/goreleaser/internal/tmpl"
	"github.com/goreleaser/goreleaser/pkg/context"
)

const (
	defaultAuthor          = `GoReleaser`
	defaultColor           = "3888754"
	defaultIcon            = "https://goreleaser.com/static/avatar.png"
	defaultMessageTemplate = `{{ .ProjectName }} {{ .Tag }} is out! Check it out at {{ .ReleaseURL }}`
)

type Pipe struct{}

func (Pipe) String() string                 { return "discord" }
func (Pipe) Skip(ctx *context.Context) bool { return !ctx.Config.Announce.Discord.Enabled }

type Config struct {
	WebhookID    string `env:"DISCORD_WEBHOOK_ID,notEmpty"`
	WebhookToken string `env:"DISCORD_WEBHOOK_TOKEN,notEmpty"`
}

func (p Pipe) Default(ctx *context.Context) error {
	if ctx.Config.Announce.Discord.MessageTemplate == "" {
		ctx.Config.Announce.Discord.MessageTemplate = defaultMessageTemplate
	}
	if ctx.Config.Announce.Discord.IconURL == "" {
		ctx.Config.Announce.Discord.IconURL = defaultIcon
	}
	if ctx.Config.Announce.Discord.Author == "" {
		ctx.Config.Announce.Discord.Author = defaultAuthor
	}
	if ctx.Config.Announce.Discord.Color == "" {
		ctx.Config.Announce.Discord.Color = defaultColor
	}
	return nil
}

func (p Pipe) Announce(ctx *context.Context) error {
	msg, err := tmpl.New(ctx).Apply(ctx.Config.Announce.Discord.MessageTemplate)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}

	var cfg Config
	if err = env.Parse(&cfg); err != nil {
		return fmt.Errorf("discord: %w", err)
	}

	log.Infof("posting: '%s'", msg)

	webhookID, err := snowflake.Parse(cfg.WebhookID)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}

	color, err := strconv.Atoi(ctx.Config.Announce.Discord.Color)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	if _, err = webhook.New(webhookID, cfg.WebhookToken).CreateMessage(discord.WebhookMessageCreate{
		Embeds: []discord.Embed{
			{
				Author: &discord.EmbedAuthor{
					Name:    ctx.Config.Announce.Discord.Author,
					IconURL: ctx.Config.Announce.Discord.IconURL,
				},
				Description: msg,
				Color:       color,
			},
		},
	}); err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	return nil
}
