// Copyright 2026 Aria Kamani and contributors. Licensed under Apache-2.0. See LICENSE.
// Novel command: the Garmin loopback SSO sign-in. Implemented in place from
// the printing-press novel-command scaffold; `generate --force` preserves an
// implemented body. Shared auth machinery lives in garmin_auth.go.
// pp:data-source live

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mvanhorn/printing-press-library/library/health/garmin/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/health/garmin/internal/config"
	"github.com/spf13/cobra"
)

func newNovelAuthLoginCmd(flags *rootFlags) *cobra.Command {
	var (
		flagEmail    string
		flagNoLogout bool
		flagTimeout  time.Duration
		flagDomain   string
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through Garmin's own page in your browser and store this account's tokens",
		Long: "Sign in through Garmin's own page in your browser and store this account's tokens.\n\n" +
			"Garmin issues no personal API token, so this is the only way in. The command signs the\n" +
			"browser out of Garmin first, opens Garmin's sign-in page, catches the one-time ticket on a\n" +
			"loopback port, exchanges it for a token pair, and then asks Garmin which account it just\n" +
			"authenticated. If that account is not the address you passed to --email, nothing is stored.\n\n" +
			"Your password is never seen, stored, or transmitted by this CLI.\n\n" +
			"One Garmin account per home: give each additional household account its own GARMIN_HOME\n" +
			"(or --home) and run this command once inside it.",
		Example: "  garmin-pp-cli auth login --email you@example.com\n" +
			"  GARMIN_HOME=~/.local/share/garmin-homes/second garmin-pp-cli auth login --email other@example.com",
		Annotations: map[string]string{"mcp:hidden": "true", "mcp:read-only": "false", "pp:data-source": "live"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return writeDryRun(cmd.OutOrStdout(), flags, "auth login")
			}
			email := strings.TrimSpace(flagEmail)
			if email == "" {
				return authErr(errors.New("--email is required: it pre-fills Garmin's form and is the account this command asserts you signed in as"))
			}
			if flagTimeout <= 0 {
				return authErr(fmt.Errorf("--timeout must be positive, got %s", flagTimeout))
			}
			if err := garminCheckDomain(flagDomain); err != nil {
				return authErr(err)
			}
			return runGarminLogin(cmd, flags, garminLoginOptions{
				Email:    email,
				Domain:   flagDomain,
				NoLogout: flagNoLogout,
				Timeout:  flagTimeout,
			})
		},
	}
	cmd.Flags().StringVar(&flagEmail, "email", "", "The Garmin account email to sign in as; the stored token is discarded if Garmin reports a different account")
	cmd.Flags().BoolVar(&flagNoLogout, "no-logout", false, "Skip the browser sign-out step (only for a browser you know has no Garmin session)")
	cmd.Flags().DurationVar(&flagTimeout, "timeout", garminLoginTimeoutDefault, "How long to wait for the browser sign-in to come back")
	cmd.Flags().StringVar(&flagDomain, "domain", garminDomainGlobal, "Garmin deployment to sign in to (garmin.com or garmin.cn)")
	return cmd
}

type garminLoginOptions struct {
	Email    string
	Domain   string
	NoLogout bool
	Timeout  time.Duration
}

func runGarminLogin(cmd *cobra.Command, flags *rootFlags, opts garminLoginOptions) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	if _, err := garminMigrateResolved(flags.configPath, out); err != nil {
		return configErr(fmt.Errorf("migrating the existing token file: %w", err))
	}
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return configErr(err)
	}
	ep := garminEndpointsFor(cfg, opts.Domain)

	// A home is bound to one Garmin account. Signing a different account into
	// the same home would mix two people's data in one archive.
	priorIdentity, _ := garminLoadIdentity(cfg.Path)
	if priorIdentity != nil && priorIdentity.Email != "" && !strings.EqualFold(priorIdentity.Email, opts.Email) {
		return authErr(fmt.Errorf(
			"this home is already signed in as %s, not %s.\n"+
				"  One Garmin account per home. Either run `garmin-pp-cli auth logout` here first,\n"+
				"  or give the other account its own home with GARMIN_HOME=<dir> or --home <dir>",
			priorIdentity.Email, opts.Email))
	}

	// 1. Sign the browser out of Garmin. The session cookies live in the
	//    browser, so this has to be a browser navigation, not an HTTP call
	//    from here: fetching the URL from Go would clear nothing.
	if !opts.NoLogout {
		if err := garminBrowserSignOut(cmd, flags, ep); err != nil {
			return authErr(err)
		}
	} else {
		fmt.Fprintln(out, "Skipping the browser sign-out step (--no-logout).")
	}

	// 2. Bind the loopback listener and open Garmin's sign-in page.
	state, err := garminNewState()
	if err != nil {
		return authErr(err)
	}
	cb, err := garminStartCallback(state)
	if err != nil {
		return authErr(err)
	}
	defer func() { _ = cb.Close() }()

	ssoURL := garminBuildSSOURL(ep, cb.URL, opts.Email)
	if cliutil.IsVerifyEnv() {
		fmt.Fprintf(out, "would launch: %s\n", ssoURL)
		return nil
	}
	fmt.Fprintf(out, "Opening Garmin's sign-in page for %s in your browser.\n", opts.Email)
	if err := openSetupURL(ssoURL); err != nil {
		fmt.Fprintf(errOut, "could not open a browser automatically: %v\n", err)
		fmt.Fprintf(out, "Open this URL yourself to continue:\n  %s\n", ssoURL)
	}
	fmt.Fprintf(out, "If the form is already filled in with another account, sign out at %s and run this again.\n", ep.SSOLogout)
	fmt.Fprintf(out, "Waiting up to %s for the sign-in to come back.\n", opts.Timeout)

	// 3. Wait for exactly one ticket. The listener stays open for the whole
	//    deadline: a callback that arrives at a closed port shows the browser
	//    an error page even though Garmin has signed the user in.
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.Timeout)
	defer cancel()
	var ticket string
	select {
	case ticket = <-cb.Ticket():
	case <-ctx.Done():
		return authErr(garminLoginTimeoutError(opts.Timeout, cb.Rejected()))
	}
	_ = cb.Close()

	// 4. Exchange the single-use ticket. One attempt only: a failed exchange
	//    burns the ticket, so retrying would fail for a second reason.
	hc := garminHTTPClient()
	tokens, err := garminExchangeServiceTicket(ctx, hc, ep, ticket, cb.URL)
	if err != nil {
		return authErr(err)
	}

	// 5. Assert the identity before anything is written.
	assertedEmail, err := garminAssertEmail(ctx, hc, ep, tokens.AccessToken, opts.Email)
	if err != nil {
		if errors.Is(err, errGarminIdentityMismatch) {
			return authErr(fmt.Errorf(
				"signed in as %s, but you asked for %s. Nothing was stored.\n"+
					"  Sign out at %s in your browser, then run this command again",
				assertedEmail, opts.Email, ep.SSOLogout))
		}
		return authErr(fmt.Errorf("could not confirm which Garmin account was signed in, so nothing was stored: %w", err))
	}

	guid := garminJWTString(tokens.AccessToken, "garmin_guid")
	if priorIdentity != nil && priorIdentity.GarminGUID != "" && guid != "" && priorIdentity.GarminGUID != guid {
		return authErr(fmt.Errorf(
			"this home is bound to a different Garmin account id than the one that just signed in. Nothing was stored.\n" +
				"  Run `garmin-pp-cli auth logout` here first, or use a separate home for the other account"))
	}

	// profileId is a second read, so it is best-effort: a login that proved
	// the account email is complete without it.
	profileID, displayName := "", ""
	time.Sleep(300 * time.Millisecond)
	var social garminSocialProfile
	if err := garminGetJSON(ctx, hc, ep, tokens.AccessToken, garminSocialProfilePath, &social); err == nil {
		profileID = social.ProfileID.String()
		displayName = social.DisplayName
	}

	// 6. Persist under the lock so a concurrent refresh cannot interleave.
	err = garminWithAuthLock(cfg.Path, func() error {
		saveCfg, err := config.Load(flags.configPath)
		if err != nil {
			return err
		}
		if err := saveCfg.SaveTokens(tokens.ClientID, "", tokens.AccessToken, tokens.RefreshToken, tokens.TokenExpiry); err != nil {
			return err
		}
		return garminSaveIdentity(saveCfg.Path, &garminIdentity{
			Email:         assertedEmail,
			GarminGUID:    guid,
			ProfileID:     profileID,
			DisplayName:   displayName,
			Domain:        ep.Domain,
			ClientID:      tokens.ClientID,
			RefreshExpiry: tokens.RefreshExpiry,
			AssertedAt:    time.Now().UTC(),
		})
	})
	if err != nil {
		return configErr(fmt.Errorf("storing the Garmin tokens: %w", err))
	}

	if flags.asJSON {
		return printJSONFiltered(out, map[string]any{
			"authenticated": true,
			"verified":      true,
			"email":         assertedEmail,
			"profile_id":    profileID,
			"config":        cfg.Path,
			"token_expiry":  garminFormatTime(tokens.TokenExpiry),
		}, flags)
	}
	fmt.Fprintln(out, green("Signed in to Garmin Connect"))
	fmt.Fprintf(out, "  Account: %s\n", assertedEmail)
	if profileID != "" {
		fmt.Fprintf(out, "  Profile: %s\n", profileID)
	}
	fmt.Fprintf(out, "  Home:    %s\n", garminHomeRungLine())
	fmt.Fprintf(out, "  Token expires: %s\n", garminFormatTime(tokens.TokenExpiry))
	return nil
}

// garminBrowserSignOut navigates the browser to Garmin's CAS logout route and,
// when someone is watching, waits for them to confirm the sign-in form came up
// empty. The route was probed live on 2026-09-07: HTTP 200, body
// "<p>Logged out!</p>". Neither reference client implements a logout at all,
// so this is verified by probe rather than borrowed.
func garminBrowserSignOut(cmd *cobra.Command, flags *rootFlags, ep garminEndpoints) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Signing your browser out of Garmin first: %s\n", ep.SSOLogout)
	if cliutil.IsVerifyEnv() {
		fmt.Fprintf(out, "would launch: %s\n", ep.SSOLogout)
		return nil
	}
	if err := openSetupURL(ep.SSOLogout); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "could not open a browser automatically: %v\n", err)
		fmt.Fprintf(out, "Open this URL yourself to sign out:\n  %s\n", ep.SSOLogout)
	}
	if flags.noInput || flags.yes || !garminInteractive(cmd) {
		fmt.Fprintln(out, "Confirm the page says you are signed out before the sign-in form appears; re-run with --no-logout to skip this step.")
		return nil
	}
	fmt.Fprint(out, "Press Enter once the browser shows you are signed out (Ctrl-C to abort): ")
	reader := bufio.NewReader(cmd.InOrStdin())
	if _, err := reader.ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("waiting for the sign-out confirmation: %w", err)
	}
	return nil
}

// garminInteractive reports whether a person is at the terminal to answer the
// sign-out prompt. Both ends matter: stdin must be a terminal to read from and
// stdout must be one for the prompt to be seen.
func garminInteractive(cmd *cobra.Command) bool {
	if !isTerminal(cmd.OutOrStdout()) {
		return false
	}
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		fi, err := f.Stat()
		if err != nil {
			return false
		}
		return (fi.Mode() & os.ModeCharDevice) != 0
	}
	return false
}

// garminLoginTimeoutError is the message for a sign-in that never came back.
// It must say that Garmin may have signed the user in regardless: a callback
// that lands after the deadline shows an error page in the browser while the
// Garmin session is live, and the next attempt then signs in the wrong
// account silently unless the user signs out first.
func garminLoginTimeoutError(timeout time.Duration, rejected int) error {
	return fmt.Errorf(
		"the browser sign-in did not come back within %s (%d callback(s) rejected for a bad nonce).\n"+
			"  Garmin may have signed you in anyway — sign out in the browser before retrying",
		timeout, rejected)
}

func garminFormatTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
