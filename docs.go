package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/config"
	"wanctl/internal/relay"
)

// cmdDocs implements `wanctl docs ...`. Reads use the relay's public endpoints
// (no token needed); writes go to the namespace-token-gated endpoints on the
// relay with the user's stored token.
func cmdDocs(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return docsUsage()
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "ls":
		return docsList(ctx, rest)
	case "get":
		return docsGet(ctx, rest)
	case "new":
		return docsNew(ctx, rest)
	case "edit":
		return docsEdit(ctx, rest)
	case "rm":
		return docsRm(ctx, rest)
	case "groups":
		return docsGroupsList(ctx)
	case "group":
		return docsGroupSub(ctx, rest)
	case "-h", "--help", "help":
		return docsUsage()
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", sub, docsHelp)
	}
}

const docsHelp = `wanctl docs subcommands:
  ls [--group SLUG]                          list articles
  get <slug>                                 print body to stdout
  new --slug S --title T --group G [--file F | --editor | < stdin]
  edit <slug> [--file F | --editor | < stdin]
  rm <slug>
  groups                                     list groups
  group new --slug S --title T [--position N]
  group rm <slug>`

func docsUsage() error { fmt.Println(docsHelp); return nil }

// --- relay endpoint helpers ---

func relayBase() string {
	return strings.TrimRight(config.EnvOr("WANCTL_RELAY", config.DefaultRelay), "/")
}

func relayToken() (string, error) {
	t := config.EnvOr("WANCTL_TOKEN", config.StoredToken())
	if t == "" {
		return "", fmt.Errorf("没有可用 token；先运行 `wanctl login` 完成飞书授权")
	}
	return t, nil
}

func relayGET(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", relayBase()+path, nil)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func relayPOST(ctx context.Context, path string, body any) error {
	tok, err := relayToken()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", relayBase()+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	admission.SetBearer(req, tok)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// --- article subcommands ---

func docsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docs ls", flag.ExitOnError)
	group := fs.String("group", "", "filter by group slug")
	fs.Parse(args)
	var tree struct{ Groups []relay.DocGroup }
	if err := relayGET(ctx, "/docs/tree.json", &tree); err != nil {
		return err
	}
	any := false
	for _, g := range tree.Groups {
		if *group != "" && g.Slug != *group {
			continue
		}
		fmt.Printf("[%s] %s\n", g.Slug, g.Title)
		for _, a := range g.Articles {
			fmt.Printf("  %-30s  %s\n", a.Slug, a.Title)
			any = true
		}
	}
	if !any {
		fmt.Println("(没有文章)")
	}
	return nil
}

func docsGet(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wanctl docs get <slug>")
	}
	var a relay.DocArticle
	if err := relayGET(ctx, "/docs/"+args[0]+".json", &a); err != nil {
		return err
	}
	fmt.Print(a.Body)
	if !strings.HasSuffix(a.Body, "\n") {
		fmt.Println()
	}
	return nil
}

func docsNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docs new", flag.ExitOnError)
	slug := fs.String("slug", "", "URL slug (unique)")
	title := fs.String("title", "", "human title")
	group := fs.String("group", "", "group slug")
	pos := fs.Int("position", 0, "sort order within group")
	file := fs.String("file", "", "read body from this file")
	editor := fs.Bool("editor", false, "open $EDITOR to write the body")
	fs.Parse(args)
	if *slug == "" || *title == "" {
		return fmt.Errorf("usage: wanctl docs new --slug S --title T [--group G] [--file F | --editor | < stdin]")
	}
	body, err := loadBody(*file, *editor, "# "+*title+"\n\n")
	if err != nil {
		return err
	}
	return relayPOST(ctx, "/docs/articles", map[string]any{
		"slug": *slug, "title": *title, "group_slug": *group, "position": *pos, "body": body,
	})
}

func docsEdit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("docs edit", flag.ExitOnError)
	file := fs.String("file", "", "read body from this file")
	editor := fs.Bool("editor", false, "open $EDITOR to edit the body")
	title := fs.String("title", "", "new title (default: keep existing)")
	group := fs.String("group", "", "move to this group (default: keep)")
	pos := fs.Int("position", -1, "new position (default: keep)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: wanctl docs edit <slug> [--file F | --editor | < stdin] [--title T] [--group G] [--position N]")
	}
	slug := fs.Arg(0)
	var a relay.DocArticle
	if err := relayGET(ctx, "/docs/"+slug+".json", &a); err != nil {
		return fmt.Errorf("查询原文章失败: %w", err)
	}
	newBody, err := loadBody(*file, *editor, a.Body)
	if err != nil {
		return err
	}
	if *title != "" {
		a.Title = *title
	}
	if *group != "" {
		a.GroupSlug = *group
		a.GroupID = 0 // force relay to look up by slug
	}
	if *pos >= 0 {
		a.Position = *pos
	}
	a.Body = newBody
	return relayPOST(ctx, "/docs/articles", map[string]any{
		"slug": a.Slug, "title": a.Title, "group_slug": a.GroupSlug,
		"position": a.Position, "body": a.Body,
	})
}

func docsRm(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wanctl docs rm <slug>")
	}
	return relayPOST(ctx, "/docs/articles/delete", map[string]string{"slug": args[0]})
}

// --- group subcommands ---

func docsGroupsList(ctx context.Context) error {
	var tree struct{ Groups []relay.DocGroup }
	if err := relayGET(ctx, "/docs/tree.json", &tree); err != nil {
		return err
	}
	if len(tree.Groups) == 0 {
		fmt.Println("(没有分组)")
		return nil
	}
	for _, g := range tree.Groups {
		fmt.Printf("%-3d %-20s %s\n", g.Position, g.Slug, g.Title)
	}
	return nil
}

func docsGroupSub(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wanctl docs group [new|rm] ...")
	}
	switch args[0] {
	case "new":
		fs := flag.NewFlagSet("docs group new", flag.ExitOnError)
		slug := fs.String("slug", "", "URL slug (unique)")
		title := fs.String("title", "", "human title")
		pos := fs.Int("position", 0, "sort order")
		fs.Parse(args[1:])
		if *slug == "" || *title == "" {
			return fmt.Errorf("usage: wanctl docs group new --slug S --title T [--position N]")
		}
		return relayPOST(ctx, "/docs/groups", map[string]any{
			"slug": *slug, "title": *title, "position": *pos,
		})
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: wanctl docs group rm <slug>")
		}
		return relayPOST(ctx, "/docs/groups/delete", map[string]string{"slug": args[1]})
	default:
		return fmt.Errorf("usage: wanctl docs group [new|rm] ...")
	}
}

// --- body input helpers ---

// loadBody resolves the article body from --file, --editor, or stdin (in that
// priority). seed is used as the editor's starting content.
func loadBody(file string, editor bool, seed string) (string, error) {
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		return string(b), nil
	case editor:
		ed := os.Getenv("EDITOR")
		if ed == "" {
			ed = "vi"
		}
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("wanctl-doc-%d.md", time.Now().UnixNano()))
		if err := os.WriteFile(tmp, []byte(seed), 0o600); err != nil {
			return "", err
		}
		defer os.Remove(tmp)
		cmd := exec.Command(ed, tmp)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("editor exited: %w", err)
		}
		b, err := os.ReadFile(tmp)
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		// Read whatever's on stdin (works for both piped input and EOF on empty).
		st, _ := os.Stdin.Stat()
		if st.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("缺少正文：用 --file、--editor 或管道把 markdown 喂进来")
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}
