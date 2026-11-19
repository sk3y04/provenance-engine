package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/slskd"
)

// slskd settings flags. The values are pointers so "not supplied" is
// distinguishable from "set to empty".
var (
	flagSlskdBaseURL   string
	flagSlskdURLBase   string
	flagSlskdAPIKey    string
	flagSlskdWebhook   string
	flagSlskdDownloads string
	flagSlskdMount     string
	flagSlskdShareRoot string
	flagSlskdSettings  string
	flagSlskdEnable    bool
	flagSlskdDisable   bool
	flagSlskdInsecure  bool
	flagSlskdCleanup   bool
)

func soulseekCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "soulseek",
		Short: "Search, download and share through an external slskd instance",
		Long: `Integrate Soulseek through an external, unmodified slskd >= 0.26.0.

Provenance never stores Soulseek credentials and never writes slskd
configuration: the Soulseek login, listen port, upload quotas and share list
stay in slskd, while Provenance keeps only a connection profile, durable
download jobs and the provenance records of what was imported.`,
	}
	cmd.PersistentFlags().StringVar(&flagSlskdSettings, "settings", "", "path to the slskd settings file (default: $XDG_CONFIG_HOME/provenance/slskd-settings.json)")
	cmd.AddCommand(soulseekSettingsCmd())
	cmd.AddCommand(soulseekHealthCmd())
	cmd.AddCommand(soulseekTestCmd())
	cmd.AddCommand(soulseekSearchCmd())
	cmd.AddCommand(soulseekDownloadCmd())
	cmd.AddCommand(soulseekJobsCmd())
	cmd.AddCommand(soulseekIngestCmd())
	cmd.AddCommand(soulseekWatchCmd())
	return cmd
}

func soulseekSettingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show or change the slskd connection profile",
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the stored profile with secrets masked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadSlskdSettings()
			if err != nil {
				return err
			}
			path, err := slskdSettingsPath()
			if err != nil {
				return err
			}
			fmt.Printf("settings file: %s\n", path)
			for _, line := range s.Describe() {
				fmt.Printf("  %s\n", line)
			}
			return nil
		},
	}

	set := &cobra.Command{
		Use:   "set",
		Short: "Update the stored profile (secrets are written 0600)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := slskdSettingsPath()
			if err != nil {
				return err
			}
			stored, err := slskd.LoadSettings(path)
			if err != nil {
				return err
			}
			update := stored
			changed := cmd.Flags().Changed
			if changed("base-url") {
				update.BaseURL = flagSlskdBaseURL
			}
			if changed("url-base") {
				update.URLBase = flagSlskdURLBase
			}
			if changed("api-key") {
				update.APIKey = flagSlskdAPIKey
			}
			if changed("webhook-secret") {
				update.WebhookSecret = flagSlskdWebhook
			}
			if changed("slskd-downloads-root") {
				update.SlskdDownloadsRoot = flagSlskdDownloads
			}
			if changed("downloads-mount") {
				update.ProvenanceDownloadsMount = flagSlskdMount
			}
			if changed("curated-share-root") {
				update.CuratedShareRoot = flagSlskdShareRoot
			}
			if changed("insecure") {
				update.InsecureSkipVerify = flagSlskdInsecure
			}
			if changed("cleanup-staging") {
				update.PostImportCleanup = flagSlskdCleanup
			}
			if flagSlskdEnable && flagSlskdDisable {
				return errors.New("--enable and --disable are mutually exclusive")
			}
			if flagSlskdEnable {
				update.Enabled = true
			}
			if flagSlskdDisable {
				update.Enabled = false
			}
			// Accept the masked value back without erasing the stored secret.
			update = update.KeepSecrets(stored)
			if err := update.Validate(); err != nil {
				return err
			}
			if err := slskd.SaveSettings(path, update); err != nil {
				return err
			}
			for _, line := range update.Describe() {
				fmt.Printf("  %s\n", line)
			}
			return nil
		},
	}
	set.Flags().StringVar(&flagSlskdBaseURL, "base-url", "", "slskd origin, e.g. http://127.0.0.1:5030")
	set.Flags().StringVar(&flagSlskdURLBase, "url-base", "", "reverse-proxy path prefix, e.g. /slskd")
	set.Flags().StringVar(&flagSlskdAPIKey, "api-key", "", "API key sent as X-API-Key (masked on read)")
	set.Flags().StringVar(&flagSlskdWebhook, "webhook-secret", "", "shared secret for inbound webhooks (masked on read)")
	set.Flags().StringVar(&flagSlskdDownloads, "slskd-downloads-root", "", "download root as slskd sees it, e.g. /downloads")
	set.Flags().StringVar(&flagSlskdMount, "downloads-mount", "", "the same directory as Provenance sees it")
	set.Flags().StringVar(&flagSlskdShareRoot, "curated-share-root", "", "isolated share staging root mounted read-only into slskd")
	set.Flags().BoolVar(&flagSlskdEnable, "enable", false, "enable the integration")
	set.Flags().BoolVar(&flagSlskdDisable, "disable", false, "disable the integration")
	set.Flags().BoolVar(&flagSlskdInsecure, "insecure", false, "disable TLS verification (self-signed reverse proxy)")
	set.Flags().BoolVar(&flagSlskdCleanup, "cleanup-staging", false, "delete the slskd staging copy after a verified import")

	cmd.AddCommand(show, set)
	return cmd
}

func soulseekHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Probe slskd and print the four-state health report",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadSlskdSettings()
			if err != nil {
				return err
			}
			h := slskd.CheckSettingsHealth(cmd.Context(), s)
			for _, line := range h.Summary() {
				fmt.Println(line)
			}
			if h.State != slskd.HealthHealthy {
				return fmt.Errorf("slskd health: %s", h.State)
			}
			return nil
		},
	}
}

func soulseekTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Test the connection using the stored profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := loadSlskdSettings()
			if err != nil {
				return err
			}
			h := slskd.CheckSettingsHealth(cmd.Context(), s)
			for _, line := range h.Summary() {
				fmt.Println(line)
			}
			if h.State != slskd.HealthHealthy {
				return fmt.Errorf("connection test failed: %s", h.State)
			}
			fmt.Println("connection OK")
			return nil
		},
	}
}

// loadSlskdSettings reads the profile and layers environment overrides on top,
// which is how a headless deployment supplies secrets without writing them.
func loadSlskdSettings() (slskd.Settings, error) {
	path, err := slskdSettingsPath()
	if err != nil {
		return slskd.Settings{}, err
	}
	s, err := slskd.LoadSettings(path)
	if err != nil {
		return slskd.Settings{}, err
	}
	return s.WithEnv(), nil
}

func slskdSettingsPath() (string, error) {
	if strings.TrimSpace(flagSlskdSettings) != "" {
		return flagSlskdSettings, nil
	}
	return slskd.DefaultSettingsPath()
}

// slskdClient builds a client from the effective profile. Kept separate so the
// commands that follow (search, jobs, shares) share one construction path.
func slskdClient(ctx context.Context) (*slskd.Client, slskd.Settings, error) {
	_ = ctx
	s, err := loadSlskdSettings()
	if err != nil {
		return nil, slskd.Settings{}, err
	}
	if !s.Enabled {
		return nil, s, errors.New("the slskd integration is disabled; run `provenance soulseek settings set --enable`")
	}
	if err := s.ValidateForTest(); err != nil {
		return nil, s, err
	}
	c, err := slskd.New(s.ClientConfig())
	if err != nil {
		return nil, s, err
	}
	return c, s, nil
}
