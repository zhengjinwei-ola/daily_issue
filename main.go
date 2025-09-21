package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CLI usage (env vars):
//
//	GITHUB_TOKEN       required: GitHub personal access token (repo scope to create issues)
//	GITHUB_OWNER       required: repository owner/org
//	GITHUB_REPO        required: repository name
//	TIMEZONE           optional: IANA TZ like "Asia/Shanghai" (default)
//	TITLE_PREFIX       optional: default "项目日报"
//	SLACK_WEBHOOK_URL  optional: Slack Incoming Webhook to notify when issue created
//	RUN_LOG_FILE       optional: path to append daily run result logs (default logs/daily_run.log)
func main() {
	ctx := context.Background()
	loadDotEnvFiles()

	owner := strings.TrimSpace(os.Getenv("GITHUB_OWNER"))
	repo := strings.TrimSpace(os.Getenv("GITHUB_REPO"))
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if owner == "" || repo == "" || token == "" {
		fmt.Println("missing env: GITHUB_OWNER, GITHUB_REPO, or GITHUB_TOKEN")
		os.Exit(1)
	}

	tzName := os.Getenv("TIMEZONE")
	if tzName == "" {
		tzName = "Asia/Shanghai"
	}

	titlePrefix := os.Getenv("TITLE_PREFIX")
	if titlePrefix == "" {
		titlePrefix = "服务端个人日报"
	}

	// Run every day at 10:00 in UTC+8 (Asia/Shanghai)
	scheduleLoc, _ := time.LoadLocation("Asia/Shanghai")
	for {
		now := time.Now().In(scheduleLoc)
		next := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, scheduleLoc)
		if !now.Before(next) {
			// If it's already 10:00 or later, schedule for tomorrow
			tomorrow := now.AddDate(0, 0, 1)
			next = time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 10, 0, 0, 0, scheduleLoc)
		}
		sleep := time.Until(next)
		fmt.Printf("waiting until %s (UTC+8) to run...\n", next.Format(time.RFC3339))
		time.Sleep(sleep)

		issueURL, created, err := createDailyReportIssue(ctx, token, owner, repo, tzName, titlePrefix)
		if err != nil {
			fmt.Println("error:", err)
			_ = appendRunLog(getRunLogPath(), fmt.Sprintf("ERROR %s: %v", time.Now().Format(time.RFC3339), err))
			continue
		}

		if created {
			fmt.Println("created:", issueURL)
			_ = appendRunLog(getRunLogPath(), fmt.Sprintf("CREATED %s: %s", time.Now().Format(time.RFC3339), issueURL))
			if webhook := strings.TrimSpace(os.Getenv("SLACK_WEBHOOK_URL")); webhook != "" {
				_ = notifySlack(webhook, fmt.Sprintf("今日日报已创建：%s", issueURL))
			}
		} else if issueURL == "" {
			fmt.Println("skipped: not a China mainland workday")
			_ = appendRunLog(getRunLogPath(), fmt.Sprintf("SKIPPED %s: not a China mainland workday", time.Now().Format(time.RFC3339)))
		} else {
			fmt.Println("exists:", issueURL)
			_ = appendRunLog(getRunLogPath(), fmt.Sprintf("EXISTS %s: %s", time.Now().Format(time.RFC3339), issueURL))
		}
	}
}

// GetStartOfDayUnixByOffsetX10 returns the Unix timestamp (seconds) of 00:00 at
// the timezone specified by a fixed UTC offset provided as hours*10.
// Example: UTC+8 => 80, UTC+5:30 => 55, UTC-3:30 => -35.
// The input timestamp can be in seconds or milliseconds; milliseconds are detected automatically.
func GetStartOfDayUnixByOffsetX10(timestamp int64, offsetX10 int) (int64, error) {
	if offsetX10 < -140 || offsetX10 > 140 {
		return 0, fmt.Errorf("invalid utc offset x10: %d (must be between -140 and 140)", offsetX10)
	}

	// Heuristic: treat values larger than 1e12 as milliseconds
	if timestamp > 1_000_000_000_000 {
		timestamp = timestamp / 1000
	}

	sign := 1
	if offsetX10 < 0 {
		sign = -1
	}
	abs := offsetX10
	if abs < 0 {
		abs = -abs
	}
	hour := abs / 10
	minutes := (abs % 10) * 6 // 0.1 hour = 6 minutes
	secondsEast := sign * (hour*3600 + minutes*60)

	signChar := '+'
	if sign < 0 {
		signChar = '-'
	}
	name := fmt.Sprintf("UTC%c%02d:%02d", signChar, hour, minutes)
	loc := time.FixedZone(name, secondsEast)

	t := time.Unix(timestamp, 0).In(loc)
	y, m, d := t.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	return start.Unix(), nil
}

// createDailyReportIssue creates or finds today's daily report issue in the given repo.
// It is idempotent: if an open issue with the same title exists, it returns its URL and created=false.
func createDailyReportIssue(ctx context.Context, token, owner, repo, tzName, titlePrefix string) (string, bool, error) {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		// fallback to UTC+8 if tz not found
		loc = time.FixedZone("UTC+08:00", 8*3600)
	}
	now := time.Now().In(loc)

	// China mainland workday check using Asia/Shanghai calendar (includes public holidays & make-up days)
	cnLoc, _ := time.LoadLocation("Asia/Shanghai")
	workday, err := isChinaWorkday(ctx, now.In(cnLoc))
	if err != nil {
		// Fallback: Mon-Fri are workdays if API unavailable
		if now.In(cnLoc).Weekday() == time.Saturday || now.In(cnLoc).Weekday() == time.Sunday {
			return "", false, nil // skip
		}
	} else if !workday {
		return "", false, nil // skip on non-workday
	}
	// Use previous China workday as the report date
	prevCN, err := getPreviousChinaWorkday(ctx, now.In(cnLoc))
	if err != nil {
		return "", false, err
	}
	y, m, d := prevCN.Date()
	dateStr := fmt.Sprintf("【%04d-%02d-%02d】", y, int(m), d)
	title := fmt.Sprintf("%s %s", dateStr, titlePrefix)

	issueURL, exists, err := findExistingIssue(ctx, token, owner, repo, title)
	if err != nil {
		return "", false, err
	}
	if exists {
		return issueURL, false, nil
	}

	body := strings.Join([]string{
		"请在此填写：",
		"- 昨日进展：",
		"- 今日计划：",
		"- 风险/阻塞：",
	}, "\n")

	url, err := createIssue(ctx, token, owner, repo, title, body)
	if err != nil {
		return "", false, err
	}
	return url, true, nil
}

func findExistingIssue(ctx context.Context, token, owner, repo, title string) (string, bool, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open&per_page=100", owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	setGitHubHeaders(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("list issues failed: %s", resp.Status)
	}
	var issues []struct {
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return "", false, err
	}
	for _, it := range issues {
		if it.Title == title {
			return it.HTMLURL, true, nil
		}
	}
	return "", false, nil
}

func createIssue(ctx context.Context, token, owner, repo, title, body string) (string, error) {
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", owner, repo)
	payload := map[string]any{
		"title": title,
		"body":  body,
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	setGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create issue failed: %s", resp.Status)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func notifySlack(webhookURL, text string) error {
	payload := map[string]any{"text": text}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook failed: %s", resp.Status)
	}
	return nil
}

// isChinaWorkday checks whether the given date (interpreted in Asia/Shanghai) is a mainland China workday.
// It uses timor.tech public holiday API which includes public holidays and make-up working days.
// If the API is unreachable or returns unknown, an error is returned and caller may fall back.
func isChinaWorkday(ctx context.Context, dateCN time.Time) (bool, error) {
	dateStr := dateCN.Format("2006-01-02")
	endpoint := os.Getenv("CHINA_WORKDAY_API")
	if endpoint == "" {
		endpoint = "https://timor.tech/api/holiday/info/" + dateStr
	} else {
		endpoint = strings.ReplaceAll(endpoint, "{date}", dateStr)
	}
	fmt.Println("endpoint:", endpoint)
	client := &http.Client{Timeout: 8 * time.Second}
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if rerr != nil {
			return false, rerr
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; DailyIssueBot/1.0; +https://github.com)")
		req.Header.Set("Referer", "https://timor.tech/")

		resp, err = client.Do(req)
		if err != nil {
			if attempt < 2 {
				time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
				continue
			}
			return false, err
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			if attempt < 2 {
				time.Sleep(time.Duration(500*(attempt+1)) * time.Millisecond)
				continue
			}
		}
		defer resp.Body.Close()
		return false, fmt.Errorf("holiday api status: %s", resp.Status)
	}
	defer resp.Body.Close()
	var out struct {
		Code int `json:"code"`
		Type *struct {
			Type int    `json:"type"` // 0 workday, 1 weekend, 2 holiday
			Name string `json:"name"`
		} `json:"type"`
		Holiday *struct {
			Holiday bool   `json:"holiday"`
			Name    string `json:"name"`
			Wage    int    `json:"wage"`
			Date    string `json:"date"`
		} `json:"holiday"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	if out.Code != 0 || out.Type == nil {
		return false, errors.New("holiday api returned unknown")
	}
	return out.Type.Type == 0, nil
}

// getRunLogPath returns the run log path from env RUN_LOG_FILE or defaults to ./logs/daily_run.log
func getRunLogPath() string {
	if p := strings.TrimSpace(os.Getenv("RUN_LOG_FILE")); p != "" {
		return p
	}
	return filepath.Join(".", "logs", "daily_run.log")
}

// appendRunLog appends one line to the run log file, creating the file and parent directories if needed.
func appendRunLog(path string, line string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err = f.WriteString(line)
	return err
}

// loadDotEnvFiles loads .env files from the working directory and from the directory of the executable.
// Precedence: existing env vars take priority; .env only fills missing keys.
func loadDotEnvFiles() {
	loadDotEnvFile(filepath.Join(".", ".env"))
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		loadDotEnvFile(filepath.Join(dir, ".env"))
	}
}

func loadDotEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// support KEY=VALUE (no export, no quotes processing)
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

// getPreviousChinaWorkday walks backwards from the given China time to find the previous workday
// according to mainland China calendar (including public holidays and make-up days).
// Returns a date at 00:00 in Asia/Shanghai.
func getPreviousChinaWorkday(ctx context.Context, dateCN time.Time) (time.Time, error) {
	cnLoc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(dateCN.Year(), dateCN.Month(), dateCN.Day(), 0, 0, 0, 0, cnLoc)
	for i := 1; i <= 31; i++ {
		candidate := start.AddDate(0, 0, -i)
		ok, err := isChinaWorkday(ctx, candidate)
		if err != nil {
			if candidate.Weekday() != time.Saturday && candidate.Weekday() != time.Sunday {
				return candidate, nil
			}
			continue
		}
		if ok {
			return candidate, nil
		}
	}
	return start.AddDate(0, 0, -1), nil
}
