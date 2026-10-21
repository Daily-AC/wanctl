// Render the real auth templates for the local visual preview.
package main

import (
	"html/template"
	"os"
	"path/filepath"
)

func main() {
	src, out := os.Args[1], os.Args[2]
	// Mail is on, as on the public instance since v0.16.0: the sign-in page
	// says an email comes next, and every signed-in account meets the email
	// door (email-*) before anything else. The confirmation link's page
	// (confirm-*) has one state per answer the relay can give.
	// The request page shows the initial in place of the avatar: the preview
	// runs offline, and the initial is exactly what a visitor sees when
	// GitHub's image host is out of reach.
	for _, page := range []struct {
		source, name, state string
		days                int
		showEmail           bool
		notify              string
	}{
		{"login", "login", "none", 0, false, ""}, {"enroll", "enroll", "none", 0, false, ""},
		{"pending", "pending", "none", 0, false, ""},
		{"email", "email", "form", 0, false, ""}, {"email", "email-sent", "sent", 0, false, "octocat@example.dev"},
		{"email-confirm", "confirm", "live", 0, false, ""}, {"email-confirm", "confirm-done", "done", 0, false, ""},
		{"email-confirm", "confirm-used", "used", 0, false, ""}, {"email-confirm", "confirm-expired", "expired", 0, false, ""},
		{"email-confirm", "confirm-unknown", "unknown", 0, false, ""},
		{"pending", "pending-sent", "pending", 0, false, "o•••@example.dev"},
		{"pending", "pending-approved", "approved", 0, false, ""}, {"pending", "pending-declined", "declined", 6, false, ""},
		{"delegation", "delegation", "pending", 0, false, ""},
		{"delegation", "delegation-done", "approved", 0, false, ""},
		{"delegation-error", "delegation-error", "", 0, false, ""},
		{"webfetch-connect", "webfetch-connect", "", 0, false, ""},
		{"webfetch-help", "webfetch-help", "", 0, false, ""},
		{"oauth", "oauth", "", 0, false, ""},
		{"oauth", "oauth-error", "error", 0, false, ""},
		{"mail-approved", "mail-approved", "", 0, false, ""}, {"mail-confirm", "mail-confirm", "", 0, false, ""},
	} {
		var status any = page.state
		var oauthError string
		if page.state == "error" {
			oauthError = "invalid_request"
		}
		if page.source == "delegation-error" {
			status = 403
		}
		t := template.Must(template.ParseFiles(filepath.Join(src, page.source+".html")))
		f, err := os.Create(filepath.Join(out, page.name+".html"))
		if err != nil {
			panic(err)
		}
		err = t.Execute(f, map[string]any{"V": "dev", "Host": "wanctl.example.dev", "Start": "/auth/github?next=%2F", "Next": "/", "Login": "octocat", "NS": "octocat", "Code": "K7RM-2QXP", "Mins": 5, "Req": page.state, "RetryDays": page.days, "NoteMax": 200, "FP": "SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw=", "MailEnabled": true, "ShowEmail": page.showEmail, "Link": "https://wanctl.example.dev/", "Initial": "o", "NotifyEmail": page.notify,
			// Same keys as handleDelegationPage / handleWebFetchConnect /
			// handleOAuthAuthorize; no alternate HTML in this preview.
			"Pending": page.state == "pending", "Status": status,
			"Request": map[string]any{"ID": "0123456789abcdef0123456789abcdef", "Label": "claude-code on studio", "ControllerFingerprint": "SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw="},
			"Devices": []map[string]any{{"Name": "DESKTOP-RQFV0SH-workstation-long", "Label": "DESKTOP-RQFV0SH-workstation-long", "Fingerprint": "SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw=", "Online": true}},
			"Relay":   "https://relay.example.com", "StartURL": "https://relay.example.com/webfetch/new/0123456789abcdef0123456789abcdef",
			"HelpURL": "https://wanctl.example.dev/webfetch/help", "SkillURL": "https://wanctl.example.dev/webfetch/skill",
			"ClientName": "Example AI client", "ClientHost": "client.example.com", "RequestID": "0123456789abcdef0123456789abcdef", "Error": oauthError,
			"Prefill": "octocat@example.dev", "SentTo": page.notify, "Address": "octocat@example.dev", "Token": "preview"})
		f.Close()
		if err != nil {
			panic(err)
		}
	}
}
