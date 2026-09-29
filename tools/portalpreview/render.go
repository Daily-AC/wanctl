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
	// names the email permission, and an applicant whose GitHub account gave
	// no verified address gets the optional email field (pending-email).
	for _, page := range []struct {
		source, name, state string
		days                int
		showEmail           bool
	}{
		{"login", "login", "none", 0, false}, {"enroll", "enroll", "none", 0, false},
		{"pending", "pending", "none", 0, false}, {"pending", "pending-email", "none", 0, true},
		{"pending", "pending-sent", "pending", 0, false},
		{"pending", "pending-approved", "approved", 0, false}, {"pending", "pending-declined", "declined", 6, false},
		{"mail-approved", "mail-approved", "", 0, false},
	} {
		t := template.Must(template.ParseFiles(filepath.Join(src, page.source+".html")))
		f, err := os.Create(filepath.Join(out, page.name+".html"))
		if err != nil {
			panic(err)
		}
		err = t.Execute(f, map[string]any{"V": "dev", "Host": "wanctl.example.dev", "Start": "/auth/github?next=%2F", "Next": "/", "Login": "octocat", "NS": "octocat", "Code": "K7RM-2QXP", "Mins": 5, "Req": page.state, "RetryDays": page.days, "NoteMax": 200, "FP": "SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw=", "MailEnabled": true, "ShowEmail": page.showEmail, "Link": "https://wanctl.example.dev/"})
		f.Close()
		if err != nil {
			panic(err)
		}
	}
}
