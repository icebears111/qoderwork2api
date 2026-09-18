package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: checkin_test <auth.json>")
		os.Exit(1)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("LOAD_ERR %v\n", err)
		os.Exit(1)
	}
	var auth struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		fmt.Printf("PARSE_ERR %v\n", err)
		os.Exit(1)
	}
	dt := auth.Auth.AccessToken
	if dt == "" {
		fmt.Printf("NO_DT\n")
		os.Exit(1)
	}

	// 直接 dt- Bearer 调签到
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("POST", "https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/claim", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("HTTP_ERR %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == 409 {
		fmt.Printf("ALREADY_CLAIMED %s\n", truncate(string(body), 120))
		return
	}
	if resp.StatusCode >= 400 {
		fmt.Printf("FAIL http_%d %s\n", resp.StatusCode, truncate(string(body), 120))
		return
	}
	fmt.Printf("OK %s\n", truncate(string(body), 200))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
