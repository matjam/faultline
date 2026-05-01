// Package imap is the IMAP-backed email adapter. It exposes a small
// connect-and-fetch surface used by the email_fetch tool. Connections are
// short-lived: callers dial, perform one operation, then Close.
package imap

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	imap "github.com/BrianLeishman/go-imap"

	"github.com/matjam/faultline/internal/config"
)

// Client wraps a go-imap dialer for the duration of a single fetch
// operation. Always created via New and Closed when done.
type Client struct {
	client *imap.Dialer
	logger *slog.Logger
}

// New connects to the IMAP server with TLS and LOGIN auth.
func New(cfg config.EmailConfig, logger *slog.Logger) (*Client, error) {
	imap.DialTimeout = 10 * time.Second
	imap.CommandTimeout = 30 * time.Second

	c, err := imap.New(cfg.User, cfg.Password, cfg.Host, cfg.Port)
	if err != nil {
		return nil, fmt.Errorf("connect to IMAP: %w", err)
	}

	return &Client{client: c, logger: logger}, nil
}

// Close releases the IMAP connection.
func (c *Client) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	c.client.Close()
	return nil
}

// FetchOverviews fetches the N most recent emails from the specified folder
// and returns a human-readable summary of envelopes (from/date/subject/UID).
func (c *Client) FetchOverviews(folder string, limit int) (string, error) {
	if err := c.client.SelectFolder(folder); err != nil {
		return "", fmt.Errorf("select %s: %w", folder, err)
	}

	uids, err := c.client.GetLastNUIDs(limit)
	if err != nil {
		return "", fmt.Errorf("get UIDs: %w", err)
	}

	if len(uids) == 0 {
		return "No emails found.", nil
	}

	overviews, err := c.client.GetOverviews(uids...)
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

// FetchBody fetches the body of a specific email by UID. Returns
// human-readable text with the headers and either the plain-text body or a
// truncated HTML excerpt.
func (c *Client) FetchBody(folder string, uid int) (string, error) {
	if err := c.client.SelectFolder(folder); err != nil {
		return "", fmt.Errorf("select %s: %w", folder, err)
	}

	emails, err := c.client.GetEmails(uid)
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
