package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	imap "github.com/BrianLeishman/go-imap"
)

// emailClient wraps an IMAP connection for email tool operations.
type emailClient struct {
	client *imap.Dialer
	logger *slog.Logger
}

// newEmailClient connects to the IMAP server with TLS and LOGIN auth.
func newEmailClient(cfg EmailConfig, logger *slog.Logger) (*emailClient, error) {
	imap.DialTimeout = 10 * time.Second
	imap.CommandTimeout = 30 * time.Second

	client, err := imap.New(cfg.User, cfg.Password, cfg.Host, cfg.Port)
	if err != nil {
		return nil, fmt.Errorf("connect to IMAP: %w", err)
	}

	return &emailClient{client: client, logger: logger}, nil
}

// fetchEmails fetches the N most recent emails from the specified folder.
func (ec *emailClient) fetchEmails(folder string, limit int) (string, error) {
	if err := ec.client.SelectFolder(folder); err != nil {
		return "", fmt.Errorf("select %s: %w", folder, err)
	}

	uids, err := ec.client.GetLastNUIDs(limit)
	if err != nil {
		return "", fmt.Errorf("get UIDs: %w", err)
	}

	if len(uids) == 0 {
		return "No emails found.", nil
	}

	overviews, err := ec.client.GetOverviews(uids...)
	if err != nil {
		return "", fmt.Errorf("get overviews: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d email(s) in %s:\n\n", len(overviews), folder)

	for uid, email := range overviews {
		fmt.Fprintf(&sb, "---\nFrom: %s\nDate: %s\nSubject: %s\n",
			email.From, email.Sent, email.Subject)
		fmt.Fprintf(&sb, "Size: %d bytes\nUID: %d\nFlags: %v\n---\n\n",
			email.Size, uid, email.Flags)
	}

	return sb.String(), nil
}

// fetchEmailBody fetches the body of a specific email by UID.
func (ec *emailClient) fetchEmailBody(folder string, uid int) (string, error) {
	if err := ec.client.SelectFolder(folder); err != nil {
		return "", fmt.Errorf("select %s: %w", folder, err)
	}

	emails, err := ec.client.GetEmails(uid)
	if err != nil {
		return "", fmt.Errorf("get email: %w", err)
	}

	email, ok := emails[uid]
	if !ok {
		return "", fmt.Errorf("email UID %d not found", uid)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "From: %s\nTo: %s\nDate: %s\nSubject: %s\n\n",
		email.From, email.To, email.Sent, email.Subject)

	if email.Text != "" {
		body := email.Text
		if len(body) > 6000 {
			body = body[:6000] + "\n... (truncated)"
		}
		sb.WriteString(body)
	} else if email.HTML != "" {
		sb.WriteString("[HTML email]\n")
		if len(email.HTML) > 2000 {
			sb.WriteString(email.HTML[:2000])
		} else {
			sb.WriteString(email.HTML)
		}
	}

	return sb.String(), nil
}

// emailFetchArgs holds the arguments for the email_fetch tool.
type emailFetchArgs struct {
	Folder string `json:"folder"`
	Limit  int    `json:"limit"`
	UID    int    `json:"uid"`
}

// emailFetch fetches recent email overviews or a specific email body from an IMAP mailbox.
func (te *ToolExecutor) emailFetch(argsJSON string) string {
	var args emailFetchArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("Error parsing arguments: %s", err)
	}

	if args.Folder == "" {
		args.Folder = "INBOX"
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}

	if te.email == nil {
		return "Error: email is not configured"
	}

	ec, err := newEmailClient(*te.email, te.logger)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	defer ec.client.Close()

	if args.UID > 0 {
		body, err := ec.fetchEmailBody(args.Folder, args.UID)
		if err != nil {
			return fmt.Sprintf("Error: %s", err)
		}
		return body
	}

	result, err := ec.fetchEmails(args.Folder, args.Limit)
	if err != nil {
		return fmt.Sprintf("Error: %s", err)
	}
	return result
}
