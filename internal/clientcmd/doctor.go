package clientcmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/harness"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	URL    string        `json:"url,omitempty"`
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

func doctorCmd(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the connection, the sign-in, and what setup configured",
		Long: `Checks that the server setup saved answers, that the sign-in works, that the
server offers Diderot's tools, and that every harness entry and skill setup
wrote is still in place, and that the background service that ships sessions,
if installed, is running and its last pass worked. It reports and changes nothing; it exits 1 when a
check fails. A harness's own OAuth sign-in lives in that harness and is not
checked.`,
		GroupID: GroupID,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError("doctor takes no arguments")
			}
			return nil
		},
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, _ []string, out *cliIO) error {
		report := runDoctor(cmd.Context(), out)
		if out.json {
			if err := out.result(report); err != nil {
				return err
			}
		} else {
			for _, c := range report.Checks {
				mark := "ok  "
				if !c.OK {
					mark = "FAIL"
				}
				out.printf("%s  %-20s %s\n", mark, c.Name, c.Detail)
			}
		}
		if !report.OK {
			return &exitError{code: exitFailure, err: errors.New("some checks failed"), reported: true}
		}
		return nil
	})
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

func runDoctor(ctx context.Context, out *cliIO) doctorReport {
	var report doctorReport
	add := func(name string, err error, okDetail string) bool {
		c := doctorCheck{Name: name, OK: err == nil, Detail: okDetail}
		if err != nil {
			c.Detail = err.Error()
		}
		report.Checks = append(report.Checks, c)
		return err == nil
	}
	path, cfg, err := loadClientConfig()
	if !add("config", err, path) {
		return report
	}
	report.URL = cfg.URL
	// Before the server's checks, which end the report when one fails: the
	// service's state matters most when the server cannot be reached.
	detail, err := sessionsCheck(ctx)
	add("sessions", err, detail)

	disc, err := client.Discover(ctx, client.NewHTTPClient(), cfg.URL)
	if err == nil && disc.Auth != cfg.Auth {
		err = fmt.Errorf("%s now signs in with %s, not the saved %s; run diderot setup %s", cfg.URL, disc.Auth, cfg.Auth, cfg.URL)
	}
	if !add("server", err, cfg.URL) {
		report.OK = false
		return finish(report)
	}

	m := out.mcp(path, cfg)
	_, err = m.Discover(ctx)
	var signIn *client.SignInError
	if errors.As(err, &signIn) {
		add("sign-in", fmt.Errorf("%w; run diderot setup %s", err, cfg.URL), "")
		return finish(report)
	}
	who := cfg.Auth
	if cfg.Account != "" {
		who = cfg.Account
	}
	add("sign-in", nil, who)
	if !add("protocol", err, "MCP "+client.ProtocolVersion) {
		return finish(report)
	}
	tools, err := m.Tools(ctx)
	if err == nil {
		var missing []string
		for _, want := range []string{"ask", "retrieve"} {
			if !slices.Contains(tools, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			err = fmt.Errorf("the server does not offer %s (it offers %s)", strings.Join(missing, ", "), strings.Join(tools, ", "))
		}
	}
	add("tools", err, strings.Join(tools, ", "))

	entry := harness.Entry{URL: cfg.URL, Token: cfg.Token, OAuth: cfg.Auth == client.AuthOAuth}
	if cfg.OAuth != nil {
		entry.ClientID = cfg.OAuth.HarnessClientID
	}
	var skillDirs []string
	for _, name := range cfg.Harnesses {
		h, ok := harness.Lookup(name)
		if !ok {
			add(name, fmt.Errorf("unknown harness %q in %s", name, path), "")
			continue
		}
		detail := "registered"
		if entry.OAuth {
			detail = "registered; its own sign-in is not checked here"
		}
		add(name, h.Check(ctx, entry), detail)
		if dir, err := h.SkillsDir(); err == nil && !slices.Contains(skillDirs, dir) {
			skillDirs = append(skillDirs, dir)
		}
	}
	for _, dir := range skillDirs {
		add("skill", harness.CheckSkill(dir), dir)
	}
	return finish(report)
}

func finish(report doctorReport) doctorReport {
	report.OK = true
	for _, c := range report.Checks {
		report.OK = report.OK && c.OK
	}
	return report
}
