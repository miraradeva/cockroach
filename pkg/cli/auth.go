// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cli

import (
	"database/sql/driver"
	"fmt"
	"net/http"
	"os"

	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login [options] <session-username>",
	Short: "create a HTTP session and token for the given user",
	Long: `
Creates a HTTP session for the given user and print out a login cookie for use
in non-interactive programs.

Example use of the session cookie using 'curl':

   curl -k -b "<cookie>" https://localhost:8080/_admin/v1/settings

The user invoking the 'login' CLI command must be an admin on the cluster.
The user for which the HTTP session is opened can be arbitrary.
`,
	Args: cobra.ExactArgs(1),
	RunE: MaybeDecorateGRPCError(runLogin),
}

func runLogin(cmd *cobra.Command, args []string) error {
	username := tree.Name(args[0]).Normalize()
	id, httpCookie, err := createAuthSessionToken(username)
	if err != nil {
		return err
	}
	hC := httpCookie.String()

	if authCtx.onlyCookie {
		// Simple format suitable for automation.
		fmt.Println(hC)
	} else {
		// More complete format, suitable e.g. for appending to a CSV file
		// with --format=csv.
		cols := []string{"username", "session ID", "authentication cookie"}
		rows := [][]string{
			{username, fmt.Sprintf("%d", id), hC},
		}
		if err := printQueryOutput(os.Stdout, cols, newRowSliceIter(rows, "ll")); err != nil {
			return err
		}

		checkInteractive(os.Stdin)
		if cliCtx.isInteractive {
			fmt.Fprintf(stderr, `#
# Example uses:
#
#     curl [-k] --cookie '%[1]s' https://...
#
#     wget [--no-check-certificate] --header='Cookie: %[1]s' https://...
#
`, hC)
		}
	}

	return nil
}

func createAuthSessionToken(username string) (sessionID int64, httpCookie *http.Cookie, err error) {
	sqlConn, err := makeSQLClient("cockroach auth-session login", useSystemDb)
	if err != nil {
		return -1, nil, err
	}
	defer sqlConn.Close()

	// First things first. Does the user exist?
	_, rows, err := runQuery(sqlConn,
		makeQuery(`SELECT count(username) FROM system.users WHERE username = $1 AND NOT "isRole"`, username), false)
	if err != nil {
		return -1, nil, err
	}
	if rows[0][0] != "1" {
		return -1, nil, fmt.Errorf("user %q does not exist", username)
	}

	// Make a secret.
	secret, hashedSecret, err := server.CreateAuthSecret()
	if err != nil {
		return -1, nil, err
	}
	expiration := timeutil.Now().Add(authCtx.validityPeriod)

	// Create the session on the server to the server.
	insertSessionStmt := `
INSERT INTO system.web_sessions ("hashedSecret", username, "expiresAt")
VALUES($1, $2, $3)
RETURNING id
`
	var id int64
	row, err := sqlConn.QueryRow(
		insertSessionStmt,
		[]driver.Value{
			hashedSecret,
			username,
			expiration,
		},
	)
	if err != nil {
		return -1, nil, err
	}
	if len(row) != 1 {
		return -1, nil, errors.Newf("expected 1 column, got %d", len(row))
	}
	id, ok := row[0].(int64)
	if !ok {
		return -1, nil, errors.Newf("expected integer, got %T", row[0])
	}

	// Spell out the cookie.
	sCookie := &serverpb.SessionCookie{ID: id, Secret: secret}
	httpCookie, err = server.EncodeSessionCookie(sCookie, false /* forHTTPSOnly */)
	return id, httpCookie, err
}

var logoutCmd = &cobra.Command{
	Use:   "logout [options] <session-username>",
	Short: "invalidates all the HTTP session tokens previously created for the given user",
	Long: `
Revokes all previously issued HTTP authentication tokens for the given user.

The user invoking the 'login' CLI command must be an admin on the cluster.
The user for which the HTTP sessions are revoked can be arbitrary.
`,
	Args: cobra.ExactArgs(1),
	RunE: MaybeDecorateGRPCError(runLogout),
}

func runLogout(cmd *cobra.Command, args []string) error {
	username := tree.Name(args[0]).Normalize()

	sqlConn, err := makeSQLClient("cockroach auth-session logout", useSystemDb)
	if err != nil {
		return err
	}
	defer sqlConn.Close()

	logoutQuery := makeQuery(
		`UPDATE system.web_sessions SET "revokedAt" = if("revokedAt"::timestamptz<now(),"revokedAt",now())
      WHERE username = $1
  RETURNING username,
            id AS "session ID",
            "revokedAt" AS "revoked"`,
		username)
	return runQueryAndFormatResults(sqlConn, os.Stdout, logoutQuery)
}

var authListCmd = &cobra.Command{
	Use:   "list",
	Short: "lists the currently active HTTP sessions",
	Long: `
Prints out the currently active HTTP sessions.

The user invoking the 'list' CLI command must be an admin on the cluster.
`,
	Args: cobra.ExactArgs(0),
	RunE: MaybeDecorateGRPCError(runAuthList),
}

func runAuthList(cmd *cobra.Command, args []string) error {
	sqlConn, err := makeSQLClient("cockroach auth-session list", useSystemDb)
	if err != nil {
		return err
	}
	defer sqlConn.Close()

	logoutQuery := makeQuery(`
SELECT username,
       id AS "session ID",
       "createdAt" as "created",
       "expiresAt" as "expires",
       "revokedAt" as "revoked",
       "lastUsedAt" as "last used"
  FROM system.web_sessions`)
	return runQueryAndFormatResults(sqlConn, os.Stdout, logoutQuery)
}

var authCmds = []*cobra.Command{
	loginCmd,
	logoutCmd,
	authListCmd,
}

var authCmd = &cobra.Command{
	Use:   "auth-session",
	Short: "log in and out of HTTP sessions",
	RunE:  usageAndErr,
}

func init() {
	authCmd.AddCommand(authCmds...)
}
