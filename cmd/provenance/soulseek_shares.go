package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sk3y04/provenance-engine/internal/slskd"
)

var (
	flagShareAlias    string
	flagSharePage     int
	flagSharePageSize int
	flagShareFilter   string
	flagShareCancel   bool
	flagUploadRemove  bool
)

func soulseekSharesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shares",
		Short: "Opt collections in and out of the curated share root",
		Long: `Manage what Provenance exposes through slskd's share scanner.

Files are materialised as same-filesystem HARD LINKS into an isolated share root
(no bytes are copied, no symlinks are ever created) after a pre-share preview
that excludes secrets and dotfiles. slskd only ever rescans; Provenance never
writes slskd's configuration over HTTP.`,
	}
	cmd.AddCommand(
		sharesListCmd(),
		sharesPreviewCmd(),
		sharesAddCmd(),
		sharesRemoveCmd(),
		sharesStatusCmd(),
		sharesRescanCmd(),
		sharesFilesCmd(),
	)
	return cmd
}

func sharesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List opted-in share rules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			rules, err := m.Store.List()
			if err != nil {
				return err
			}
			if len(rules) == 0 {
				fmt.Println("no share rules")
				return nil
			}
			for _, r := range rules {
				fmt.Printf("%-20s %-30s %6d file(s)  %8s  %s\n",
					r.ID, r.AliasFolder, r.FileCount, humanBytes(r.TotalBytes), r.SourcePath)
			}
			return nil
		},
	}
}

func sharesPreviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preview <path>",
		Short: "Show exactly what sharing a folder would expose",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			p, err := m.Preview(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("source: %s\n", p.SourcePath)
			fmt.Printf("files: %d  total: %s  excluded: %d  symlinks: %d\n",
				len(p.Files), humanBytes(p.TotalBytes), p.Excluded, p.Symlinks)
			if len(p.Extensions) > 0 {
				var parts []string
				for ext, n := range p.Extensions {
					parts = append(parts, fmt.Sprintf("%s=%d", ext, n))
				}
				fmt.Printf("extensions: %s\n", strings.Join(parts, " "))
			}
			for _, s := range p.Sample {
				fmt.Printf("  excluded %s\n", s)
			}
			fmt.Println("nothing has been linked yet; run `provenance soulseek shares add` to confirm")
			return nil
		},
	}
}

func sharesAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Confirm a share: hardlink the folder and rescan slskd",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			rule, preview, err := m.Add(cmd.Context(), args[0], flagShareAlias)
			if err != nil {
				return err
			}
			fmt.Printf("shared %s as %q: %d file(s), %s (%d excluded)\n",
				rule.SourcePath, rule.AliasFolder, rule.FileCount, humanBytes(preview.TotalBytes), preview.Excluded)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagShareAlias, "alias", "", "folder name shown to peers (default: the source folder name)")
	return cmd
}

func sharesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <ruleID>",
		Short: "Remove the materialised links and rescan slskd",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			rule, err := m.Remove(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("removed share %q\n", rule.AliasFolder)
			return nil
		},
	}
}

func sharesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what slskd reports as shared",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			statuses, err := m.Status(cmd.Context())
			if err != nil {
				return err
			}
			if len(statuses) == 0 {
				fmt.Println("slskd reports no shares")
			}
			var total int
			for _, s := range statuses {
				fmt.Printf("%-20s %-30s %6d file(s)  remote=%s\n", s.Alias, s.LocalPath, s.Files, s.RemotePath)
				total += s.Files
			}
			rules, err := m.Store.List()
			if err != nil {
				return err
			}
			enabled := 0
			for _, r := range rules {
				if r.Enabled {
					enabled++
				}
			}
			for _, w := range slskd.MisconfigurationWarnings(slskd.ShareWarningContext{EnabledRules: enabled, ShareFiles: total}) {
				fmt.Printf("warning %s: %s\n", w.Code, w.Message)
			}
			return nil
		},
	}
}

func sharesRescanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rescan",
		Short: "Ask slskd to rebuild its share index (or cancel a running scan)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			if flagShareCancel {
				if err := m.CancelScan(cmd.Context()); err != nil {
					return err
				}
				fmt.Println("share scan cancelled")
				return nil
			}
			if err := m.Rescan(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("share scan started")
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagShareCancel, "cancel", false, "cancel a running scan instead of starting one")
	return cmd
}

func sharesFilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "files",
		Short: "Browse the shared file index, paginated",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := shareManager()
			if err != nil {
				return err
			}
			page, err := m.Browse(cmd.Context(), flagSharePage, flagSharePageSize, flagShareFilter)
			if err != nil {
				return err
			}
			for _, e := range page.Entries {
				fmt.Println(e)
			}
			fmt.Printf("\npage %d/%d, %d entries, %s total\n", page.Page, page.Pages, page.TotalFiles, humanBytes(page.TotalBytes))
			return nil
		},
	}
	cmd.Flags().IntVar(&flagSharePage, "page", 1, "page number")
	cmd.Flags().IntVar(&flagSharePageSize, "page-size", 100, "entries per page")
	cmd.Flags().StringVar(&flagShareFilter, "filter", "", "substring filter")
	return cmd
}

func soulseekUploadsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uploads",
		Short: "Monitor and cancel peer uploads",
	}
	cmd.AddCommand(
		uploadsListCmd(),
		uploadsCancelCmd(),
		uploadsClearCmd(),
	)
	return cmd
}

func uploadsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active and queued uploads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			u := &slskd.Uploads{Client: client}
			list, err := u.List(cmd.Context())
			if err != nil {
				return err
			}
			sum := slskd.Summarize(list)
			for _, t := range list {
				fmt.Printf("%-12s %-20s %-50s %s\n", t.ID, t.Username, truncate(t.Filename, 50), t.State)
			}
			fmt.Printf("\nactive=%d queued=%d completed=%d failed=%d transferred=%s\n",
				sum.Active, sum.Queued, sum.Completed, sum.Failed, humanBytes(sum.Bytes))
			return nil
		},
	}
}

func uploadsCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <username> <uploadID>",
		Short: "Cancel one upload",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			u := &slskd.Uploads{Client: client}
			if err := u.Cancel(cmd.Context(), args[0], args[1], flagUploadRemove); err != nil {
				return err
			}
			fmt.Println("upload cancelled")
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagUploadRemove, "remove", false, "also remove the record")
	return cmd
}

func uploadsClearCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-completed",
		Short: "Remove completed upload records from slskd",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := slskdClient(cmd.Context())
			if err != nil {
				return err
			}
			u := &slskd.Uploads{Client: client}
			if err := u.ClearCompleted(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("cleared completed uploads")
			return nil
		},
	}
}

// shareManager builds a ShareManager from the stored profile.
func shareManager() (*slskd.ShareManager, error) {
	client, settings, err := slskdClient(context.Background())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(settings.CuratedShareRoot) == "" {
		return nil, errors.New("no curated share root is configured; run `provenance soulseek settings set --curated-share-root <path>`")
	}
	stateRoot, err := slskdStateRoot(settings)
	if err != nil {
		return nil, err
	}
	store, err := slskd.OpenShareStore(stateRoot)
	if err != nil {
		return nil, err
	}
	return &slskd.ShareManager{Root: settings.CuratedShareRoot, Store: store, Client: client}, nil
}
