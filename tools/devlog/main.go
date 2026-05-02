package main

import (
	"bytes"
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
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: devlog <daily|weekly> [flags]")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daily":
		os.Exit(runDaily(os.Args[2:]))
	case "weekly":
		os.Exit(runWeekly(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: devlog <daily|weekly> [flags]\n", os.Args[1])
		os.Exit(1)
	}
}

// --- daily ---

func runDaily(args []string) int {
	fs := flag.NewFlagSet("daily", flag.ExitOnError)
	repoPath := fs.String("repo", defaultRepoPath(), "Path to journal git repo")
	dateStr := fs.String("date", time.Now().Format("2006-01-02"), "Date to generate devlog for (YYYY-MM-DD)")
	ghUser := fs.String("user", "alcxyz", "GitHub username for activity lookup")
	fs.Parse(args)

	date, err := time.Parse("2006-01-02", *dateStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid date: %s\n", *dateStr)
		return 1
	}
	ds := date.Format("2006-01-02")

	outfile := filepath.Join(*repoPath, "devlog", ds+".md")
	if fileExists(outfile) {
		fmt.Printf("Devlog for %s already exists: %s\n", ds, outfile)
		return 0
	}

	fmt.Printf("Generating devlog for %s...\n", ds)

	commits, err := fetchCommitData(*ghUser, ds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching commits: %v\n", err)
		return 1
	}

	prs, _ := fetchSearchItems(*ghUser, ds, "pr")
	issues, _ := fetchSearchItems(*ghUser, ds, "issue")

	if commits == "" && prs == "" && issues == "" {
		mustMkdir(filepath.Dir(outfile))
		content := fmt.Sprintf("---\ndate: %s\n---\n# Devlog — %s\n\nNo activity.\n", ds, ds)
		if err := os.WriteFile(outfile, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing file: %v\n", err)
			return 1
		}
		fmt.Printf("No activity for %s.\n", ds)
	} else {
		fmt.Println("Fetching diffs...")
		diffs := fetchDiffs(commits)

		prompt := buildDailyPrompt(ds, diffs, prs, issues)
		mustMkdir(filepath.Dir(outfile))
		if err := runLLM(prompt, outfile); err != nil {
			fmt.Fprintf(os.Stderr, "error running LLM: %v\n", err)
			return 1
		}
	}

	if err := gitCommitAndPush(*repoPath, filepath.Join("devlog", ds+".md"), "devlog: "+ds); err != nil {
		fmt.Fprintf(os.Stderr, "error committing: %v\n", err)
		return 1
	}

	fmt.Printf("Done: %s\n", outfile)
	return 0
}

func fetchCommitData(user, date string) (string, error) {
	query := fmt.Sprintf("author:%s+committer-date:%s", user, date)
	jq := `.items[] | "\(.repository.full_name) \(.sha) \(.commit.message | split("\n") | first)"`
	out, err := run("gh", "api", fmt.Sprintf("search/commits?q=%s&per_page=100", query), "--jq", jq)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func fetchSearchItems(user, date, itemType string) (string, error) {
	query := fmt.Sprintf("author:%s+type:%s+updated:%s", user, itemType, date)
	jq := `.items[] | "- [\(.repository_url | split("/") | last)] #\(.number) \(.title) (\(.state))"`
	out, err := run("gh", "api", fmt.Sprintf("search/issues?q=%s&per_page=100", query), "--jq", jq)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func fetchDiffs(commitData string) string {
	if commitData == "" {
		return ""
	}

	var diffs strings.Builder
	for _, line := range strings.Split(commitData, "\n") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			continue
		}
		repo, sha, msg := parts[0], parts[1], parts[2]

		jq := `"Files changed: \(.stats.total // 0) (+\(.stats.additions // 0) -\(.stats.deletions // 0))\n" + ([.files[] | "  \(.filename) (+\(.additions) -\(.deletions))"] | join("\n")) + "\n\nPatch (truncated):\n" + ([.files[] | select(.patch != null) | "--- \(.filename)\n\(.patch)"] | join("\n"))`
		detail, err := run("gh", "api", fmt.Sprintf("repos/%s/commits/%s", repo, sha), "--jq", jq)
		if err != nil {
			detail = "(diff unavailable)"
		} else {
			// Truncate to ~200 lines
			lines := strings.Split(detail, "\n")
			if len(lines) > 200 {
				detail = strings.Join(lines[:200], "\n")
			}
		}

		fmt.Fprintf(&diffs, "\n### %s — %s\n%s\n", repo, msg, detail)
	}
	return diffs.String()
}

func buildDailyPrompt(date, diffs, prs, issues string) string {
	if prs == "" {
		prs = "None"
	}
	if issues == "" {
		issues = "None"
	}

	return fmt.Sprintf(`You are writing a daily devlog entry. You have access to actual commit diffs — use them to understand what was really done, not just the commit messages (which may be vague or uninformative).

Output ONLY the raw markdown — no code fences, no commentary, no preamble.

Format:
---
date: %s
---
# Devlog — %s

## What happened
(Narrative description of the day's work, organized by project/theme. Describe what was actually built, fixed, or changed based on the diffs. Be specific — mention file types, features, patterns. 1-2 paragraphs.)

## Changes by repo

### repo-name
- Bullet points describing each meaningful change (not just repeating commit messages — interpret the diffs)

(repeat for each repo)

## PRs
(list if any, omit section entirely if none)

## Issues
(list if any, omit section entirely if none)

Rules:
- Output starts with --- (the frontmatter delimiter), nothing before it
- Analyze the diffs to understand what actually changed, don't just parrot commit messages
- Merge related commits into a single description when they're part of the same logical change
- Be specific about what code/features were added or modified
- Keep it concise but informative

COMMIT DIFFS:
%s

PULL REQUESTS:
%s

ISSUES:
%s`, date, date, diffs, prs, issues)
}

// --- weekly ---

func runWeekly(args []string) int {
	fs := flag.NewFlagSet("weekly", flag.ExitOnError)
	repoPath := fs.String("repo", defaultRepoPath(), "Path to journal git repo")
	dateStr := fs.String("date", sevenDaysAgo(), "Any date in the target week (YYYY-MM-DD)")
	hedgedocPost := fs.Bool("hedgedoc", envBool("HEDGEDOC_POST"), "Post to HedgeDoc")
	hedgedocBin := fs.String("hedgedoc-bin", envOr("HEDGEDOC_BIN", "/home/alc/src/infra/gitops/tools/hedgedoc/hedgedoc"), "Path to hedgedoc binary")
	hedgedocSecrets := fs.String("hedgedoc-secrets", os.Getenv("HEDGEDOC_SECRETS_FILE"), "Path to sops-encrypted HedgeDoc secrets.env")
	fs.Parse(args)

	refDate, err := time.Parse("2006-01-02", *dateStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid date: %s\n", *dateStr)
		return 1
	}

	monday := weekday(refDate, time.Monday)
	friday := monday.AddDate(0, 0, 4)
	sunday := monday.AddDate(0, 0, 6)
	_, week := monday.ISOWeek()
	year, _ := monday.ISOWeek()
	weekStr := fmt.Sprintf("%d-W%02d", year, week)

	monStr := monday.Format("2006-01-02")
	friStr := friday.Format("2006-01-02")
	sunStr := sunday.Format("2006-01-02")

	outfile := filepath.Join(*repoPath, "weekly", weekStr+".md")
	if fileExists(outfile) {
		fmt.Printf("Weekly devlog for %s already exists: %s\n", weekStr, outfile)
		return 0
	}

	fmt.Printf("Generating weekly devlog for %s (%s to %s)...\n", weekStr, monStr, sunStr)

	// Collect daily entries split by weekday/weekend
	var weekdayEntries, weekendEntries strings.Builder
	hasContent := false

	for d := monday; !d.After(sunday); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		daily := filepath.Join(*repoPath, "devlog", ds+".md")
		data, err := os.ReadFile(daily)
		if err != nil {
			fmt.Printf("  No entry for %s, skipping\n", ds)
			continue
		}
		content := string(data)
		if !strings.Contains(content, "No activity.") {
			hasContent = true
		}
		if d.Weekday() >= time.Monday && d.Weekday() <= time.Friday {
			weekdayEntries.WriteString(content)
			weekdayEntries.WriteString("\n\n---\n\n")
		} else {
			weekendEntries.WriteString(content)
			weekendEntries.WriteString("\n\n---\n\n")
		}
	}

	if weekdayEntries.Len() == 0 && weekendEntries.Len() == 0 {
		fmt.Printf("No daily devlogs found for %s\n", weekStr)
		return 0
	}

	mustMkdir(filepath.Dir(outfile))

	if !hasContent {
		content := fmt.Sprintf("---\ndate: %s\nweek: %s\ntags: alcxyz, devlog, weekly\n---\n# Week %s — %s to %s\n\nNo activity.\n",
			monStr, weekStr, weekStr, monStr, sunStr)
		if err := os.WriteFile(outfile, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing file: %v\n", err)
			return 1
		}
		fmt.Printf("No activity for %s.\n", weekStr)
	} else {
		weStr := weekendEntries.String()
		if weStr == "" {
			weStr = "No weekend entries."
		}
		prompt := buildWeeklyPrompt(monStr, friStr, sunStr, weekStr, weekdayEntries.String(), weStr)
		if err := runLLM(prompt, outfile); err != nil {
			fmt.Fprintf(os.Stderr, "error running LLM: %v\n", err)
			return 1
		}

		// Append raw daily entries below the summary
		summary, err := os.ReadFile(outfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading summary: %v\n", err)
			return 1
		}

		var stitched strings.Builder
		stitched.Write(summary)
		stitched.WriteString("\n\n---\n\n# Daily entries\n")

		for d := monday; !d.After(sunday); d = d.AddDate(0, 0, 1) {
			ds := d.Format("2006-01-02")
			daily := filepath.Join(*repoPath, "devlog", ds+".md")
			data, err := os.ReadFile(daily)
			if err != nil {
				continue
			}
			stitched.WriteString("\n")
			stitched.Write(data)
			stitched.WriteString("\n\n---\n")
		}

		if err := os.WriteFile(outfile, []byte(stitched.String()), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error writing stitched file: %v\n", err)
			return 1
		}
	}

	// Post to HedgeDoc
	if *hedgedocPost {
		if err := postToHedgeDoc(outfile, *hedgedocBin, *hedgedocSecrets); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: HedgeDoc posting failed: %v\n", err)
		}
	}

	if err := gitCommitAndPush(*repoPath, filepath.Join("weekly", weekStr+".md"), "weekly: "+weekStr); err != nil {
		fmt.Fprintf(os.Stderr, "error committing: %v\n", err)
		return 1
	}

	fmt.Printf("Done: %s\n", outfile)
	return 0
}

func buildWeeklyPrompt(monday, friday, sunday, week, weekdayEntries, weekendEntries string) string {
	return fmt.Sprintf(`You are writing a weekly devlog summary. You are given daily devlog entries for the week, split into weekday (Mon-Fri) and weekend (Sat-Sun) sections.

Your job is to synthesize TWO separate summaries — one for weekdays and one for the weekend. Do NOT repeat daily details; the raw entries will be appended below your summary.

Output ONLY the raw markdown — no code fences, no commentary, no preamble.

Format:
---
date: %s
week: %s
tags: alcxyz, devlog, weekly
---
# Week %s — %s to %s

## Weekdays
(Synthesized narrative of Monday through Friday. 2-4 key themes or threads, what was accomplished, what progressed. Focus on outcomes, not activity.)

## Weekend
(Synthesized narrative of Saturday-Sunday work — what was done, how it relates to or differs from the weekday focus. If no weekend activity, write "No weekend activity." and nothing else in this section.)

## Highlights
- Bullet list of the most significant accomplishments across the whole week (shipped features, completed milestones, resolved blockers)

## Patterns & observations
(Optional — any meta-observations about work patterns, recurring issues, or strategic direction. Omit this section entirely if nothing meaningful to say.)

Rules:
- Output starts with --- (the frontmatter delimiter), nothing before it
- This is a SYNTHESIS, not a concatenation — find the narrative arc
- Do not repeat daily-level commit details; the raw daily entries are appended after your summary
- Focus on outcomes and progress, not activity
- Keep the total length under 400 words
- The tags line in frontmatter is mandatory

WEEKDAY ENTRIES (Mon-Fri):
%s

WEEKEND ENTRIES (Sat-Sun):
%s`, monday, week, week, monday, sunday, weekdayEntries, weekendEntries)
}

func postToHedgeDoc(file, bin, secretsFile string) error {
	// If credentials aren't in env, decrypt them
	if os.Getenv("HEDGEDOC_URL") == "" && secretsFile != "" {
		out, err := run("sops", "--decrypt", secretsFile)
		if err != nil {
			return fmt.Errorf("failed to decrypt secrets: %w", err)
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				v = strings.TrimSpace(v)
				v = strings.Trim(v, `"'`)
				os.Setenv(strings.TrimSpace(k), v)
			}
		}
	}

	if os.Getenv("HEDGEDOC_URL") == "" {
		return fmt.Errorf("HEDGEDOC_POST set but no credentials available")
	}

	out, err := run(bin, "post", file)
	if err != nil {
		return err
	}
	fmt.Printf("Posted to HedgeDoc: %s\n", strings.TrimSpace(out))
	return nil
}

// --- shared helpers ---

func runLLM(prompt, outfile string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	out, err := callProvider(cfg.Model, prompt)
	if err != nil {
		if cfg.Model.Backup == nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "primary provider failed (%s/%s): %v — trying backup\n",
			cfg.Model.Provider, cfg.Model.Model, err)
		out, err = callProvider(*cfg.Model.Backup, prompt)
		if err != nil {
			return fmt.Errorf("backup provider also failed (%s/%s): %w",
				cfg.Model.Backup.Provider, cfg.Model.Backup.Model, err)
		}
	}

	return os.WriteFile(outfile, out, 0644)
}

func resolveAPIKey(envName string) string {
	if envName == "" {
		return ""
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	fileEnv := envName + "_FILE"
	if path := os.Getenv(fileEnv); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func callProvider(cfg ModelConfig, prompt string) ([]byte, error) {
	apiKey := resolveAPIKey(cfg.APIKeyEnv)

	switch cfg.Provider {
	case "anthropic":
		return callWithTransport(cfg.Transport, apiKey != "", func() ([]byte, error) {
			return callAnthropicCLI(cfg.Model, prompt)
		}, func() ([]byte, error) {
			return callAnthropicAPI(cfg.Model, apiKey, prompt)
		})
	case "openai":
		return callWithTransport(cfg.Transport, apiKey != "", func() ([]byte, error) {
			return callOpenAICLI(cfg.Model, prompt)
		}, func() ([]byte, error) {
			return callOpenAIAPI(cfg.Model, apiKey, prompt)
		})
	default:
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

func callWithTransport(transport string, apiAvailable bool, cliFn, apiFn func() ([]byte, error)) ([]byte, error) {
	switch transport {
	case "cli":
		return cliFn()
	case "api":
		if !apiAvailable {
			return nil, fmt.Errorf("transport api requires api_key_env to resolve")
		}
		return apiFn()
	case "prefer-cli":
		return cliFn()
	case "prefer-api":
		if apiAvailable {
			return apiFn()
		}
		return cliFn()
	default:
		return nil, fmt.Errorf("unsupported transport: %s", transport)
	}
}

func callAnthropicCLI(model, prompt string) ([]byte, error) {
	cmd := exec.Command("claude", "-p", "--model", model)
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("claude exited %d: %s", ee.ExitCode(), string(ee.Stderr))
		}
		return nil, err
	}
	return out, nil
}

func callAnthropicAPI(model, apiKey, prompt string) ([]byte, error) {
	reqBody := map[string]interface{}{
		"model":      model,
		"max_tokens": 8192,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("anthropic API returned empty content")
	}
	return []byte(result.Content[0].Text), nil
}

func callOpenAIAPI(model, apiKey, prompt string) ([]byte, error) {
	reqBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("openai API returned no choices")
	}
	return []byte(result.Choices[0].Message.Content), nil
}

func callOpenAICLI(model, prompt string) ([]byte, error) {
	outFile, err := os.CreateTemp("", "devlog-codex-*.txt")
	if err != nil {
		return nil, fmt.Errorf("create codex output file: %w", err)
	}
	outPath := outFile.Name()
	outFile.Close()
	defer os.Remove(outPath)

	cmd := exec.Command("codex", "exec",
		"--skip-git-repo-check",
		"--ignore-rules",
		"--ephemeral",
		"-C", os.TempDir(),
		"-m", model,
		"-o", outPath,
		"-",
	)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("codex exited %d\nstderr: %s\nstdout: %s", ee.ExitCode(), stderr.String(), stdout.String())
		}
		return nil, fmt.Errorf("run codex: %w", err)
	}

	out, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read codex output: %w", err)
	}
	return bytes.TrimSpace(out), nil
}

func gitCommitAndPush(repoPath, relPath, message string) error {
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if err := git("add", relPath); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := git("commit", "-m", message); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	if err := git("push"); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func weekday(t time.Time, target time.Weekday) time.Time {
	diff := int(t.Weekday()) - int(target)
	if diff < 0 {
		diff += 7
	}
	return t.AddDate(0, 0, -diff)
}

func defaultRepoPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "git", "journal")
}

func sevenDaysAgo() string {
	return time.Now().AddDate(0, 0, -7).Format("2006-01-02")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustMkdir(path string) {
	os.MkdirAll(path, 0755)
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
