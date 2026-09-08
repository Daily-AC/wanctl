// Render the real auth templates for the local visual preview.
package main

import (
	"html/template"
	"os"
	"path/filepath"
)

func main() {
	src, out := os.Args[1], os.Args[2]
	for _, page := range []struct {
		source, name, state string
		days                int
	}{
		{"login", "login", "none", 0}, {"enroll", "enroll", "none", 0},
		{"pending", "pending", "none", 0}, {"pending", "pending-sent", "pending", 0},
		{"pending", "pending-approved", "approved", 0}, {"pending", "pending-declined", "declined", 6},
	} {
		t := template.Must(template.ParseFiles(filepath.Join(src, page.source+".html")))
		f, err := os.Create(filepath.Join(out, page.name+".html"))
		if err != nil {
			panic(err)
		}
		err = t.Execute(f, map[string]any{"V": "dev", "Host": "wanctl.example.dev", "Start": "/auth/github?next=%2F", "Next": "/", "Login": "octocat", "NS": "octocat", "Code": "K7RM-2QXP", "Mins": 5, "Req": page.state, "RetryDays": page.days, "NoteMax": 200, "FP": "SHA256:tQ8mv3ZKcR1yXpN0jbLdE7aWfHuGiO4sPzC2rYkVnBw="})
		f.Close()
		if err != nil {
			panic(err)
		}
	}
}
