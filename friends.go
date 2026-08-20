package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/relay"
)

const friendsHelp = `wanctl friends subcommands:
  friends                         list friends and pending requests
  friends add <namespace>         send a friend request
  friends accept <namespace>      accept an incoming request
  friends decline <namespace>     decline an incoming request
  friends remove <namespace>      remove a friend and revoke shares in both directions`

const shareHelp = `wanctl share subcommands:
  share list
  share grant --device DEV --to NS [--perms exec,read]
  share revoke --device DEV --to NS`

type relayHTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *relayHTTPError) Error() string {
	if e.Body == "" {
		return e.Status
	}
	return e.Status + ": " + e.Body
}

func userRequest(ctx context.Context, method, path string, body, out any) error {
	base, err := relayBase()
	if err != nil {
		return err
	}
	token, err := relayToken()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	admission.SetBearer(req, token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return &relayHTTPError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(raw)),
		}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func cmdFriends(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return friendsList(ctx)
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(friendsHelp)
		return nil
	}
	if len(args) != 2 {
		return fmt.Errorf("%s", friendsHelp)
	}
	peer := args[1]
	switch args[0] {
	case "add":
		var result struct {
			Status string `json:"status"`
		}
		err := userRequest(ctx, http.MethodPost, "/u/friends/request", map[string]string{"namespace": peer}, &result)
		if isRelayError(err, http.StatusNotFound, "no-such-user") {
			return fmt.Errorf("对方还没注册 wanctl：%s", peer)
		}
		if err != nil {
			return err
		}
		if result.Status == "accepted" {
			fmt.Printf("✓ %s 也已请求添加你，现已成为好友\n", peer)
		} else {
			fmt.Printf("✓ 已向 %s 发送好友请求\n", peer)
		}
		return nil
	case "accept":
		if err := friendPost(ctx, "/u/friends/accept", peer); err != nil {
			return err
		}
		fmt.Printf("✓ 已接受 %s 的好友请求\n", peer)
		return nil
	case "decline":
		if err := friendPost(ctx, "/u/friends/decline", peer); err != nil {
			return err
		}
		fmt.Printf("✓ 已拒绝 %s 的好友请求\n", peer)
		return nil
	case "remove":
		if err := friendPost(ctx, "/u/friends/remove", peer); err != nil {
			return err
		}
		fmt.Printf("✓ 已删除好友 %s，并撤销双方之间的所有设备共享\n", peer)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], friendsHelp)
	}
}

func friendPost(ctx context.Context, path, peer string) error {
	err := userRequest(ctx, http.MethodPost, path, map[string]string{"namespace": peer}, nil)
	if isRelayError(err, http.StatusNotFound, "no-such-friend") {
		return fmt.Errorf("没有匹配的好友关系或待处理请求：%s", peer)
	}
	return err
}

func isRelayError(err error, status int, body string) bool {
	httpErr, ok := err.(*relayHTTPError)
	return ok && httpErr.StatusCode == status && httpErr.Body == body
}

func friendsList(ctx context.Context) error {
	var result struct {
		Friends []relay.Friend `json:"friends"`
	}
	if err := userRequest(ctx, http.MethodGet, "/u/friends", nil, &result); err != nil {
		return err
	}
	printFriendSection("好友", result.Friends, func(friend relay.Friend) bool {
		return friend.Status == "accepted"
	})
	printFriendSection("待你接受", result.Friends, func(friend relay.Friend) bool {
		return friend.Status == "pending" && friend.Direction == "incoming"
	})
	printFriendSection("等待对方接受", result.Friends, func(friend relay.Friend) bool {
		return friend.Status == "pending" && friend.Direction == "outgoing"
	})
	return nil
}

func printFriendSection(title string, friends []relay.Friend, include func(relay.Friend) bool) {
	fmt.Println(title + "：")
	count := 0
	for _, friend := range friends {
		if !include(friend) {
			continue
		}
		fmt.Printf("  %-24s %s\n", friend.Namespace, friend.Since.Local().Format("2006-01-02 15:04"))
		count++
	}
	if count == 0 {
		fmt.Println("  (无)")
	}
}

func cmdShare(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", shareHelp)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: wanctl share list")
		}
		return shareList(ctx)
	case "grant":
		return shareGrant(ctx, args[1:])
	case "revoke":
		return shareRevoke(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Println(shareHelp)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q\n%s", args[0], shareHelp)
	}
}

func shareGrant(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("share grant", flag.ContinueOnError)
	device := fs.String("device", "", "device name")
	to := fs.String("to", "", "friend namespace")
	perms := fs.String("perms", "exec,read", "grant permissions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device == "" || *to == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: wanctl share grant --device DEV --to NS [--perms exec,read]")
	}
	var result struct {
		ID int `json:"id"`
	}
	err := userRequest(ctx, http.MethodPost, "/u/shares/grant", map[string]string{
		"device": *device, "grantee": *to, "perms": *perms,
	}, &result)
	if isRelayError(err, http.StatusForbidden, "not-friends") {
		return fmt.Errorf("%s 还不是你的好友；请先运行 `wanctl friends add %s`", *to, *to)
	}
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已将设备 %s 共享给 %s（授权 #%d，权限 %s）\n", *device, *to, result.ID, *perms)
	return nil
}

func shareRevoke(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("share revoke", flag.ContinueOnError)
	device := fs.String("device", "", "device name")
	to := fs.String("to", "", "friend namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device == "" || *to == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: wanctl share revoke --device DEV --to NS")
	}
	if err := userRequest(ctx, http.MethodPost, "/u/shares/revoke", map[string]string{
		"device": *device, "grantee": *to,
	}, nil); err != nil {
		return err
	}
	fmt.Printf("✓ 已撤销 %s 对设备 %s 的访问\n", *to, *device)
	return nil
}

func shareList(ctx context.Context) error {
	var result struct {
		Given []struct {
			ID      int    `json:"id"`
			Device  string `json:"device"`
			Grantee string `json:"grantee"`
			Perms   string `json:"perms"`
		} `json:"given"`
		Received []relay.ReceivedShare `json:"received"`
	}
	if err := userRequest(ctx, http.MethodGet, "/u/shares", nil, &result); err != nil {
		return err
	}
	fmt.Println("我授出的共享：")
	if len(result.Given) == 0 {
		fmt.Println("  (无)")
	}
	for _, share := range result.Given {
		fmt.Printf("  %-20s -> %-20s %s (#%d)\n", share.Device, share.Grantee, share.Perms, share.ID)
	}
	fmt.Println("好友授给我的共享：")
	if len(result.Received) == 0 {
		fmt.Println("  (无)")
	}
	for _, share := range result.Received {
		fmt.Printf("  %-20s / %-20s %s\n", share.Owner, share.Device, share.Perms)
	}
	return nil
}
