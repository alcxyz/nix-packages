package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Primary struct {
		Host string `toml:"host"`
		Port int    `toml:"port"`
	} `toml:"primary"`
	Secondary struct {
		Host string `toml:"host"`
		Port int    `toml:"port"`
	} `toml:"secondary"`
	Logging struct {
		File string `toml:"file"`
	} `toml:"logging"`
}

type AuthResponse struct {
	Session struct {
		Valid   bool    `json:"valid"`
		SID     *string `json:"sid"`
		CSRF    *string `json:"csrf"`
		Message string  `json:"message"`
	} `json:"session"`
}

type SessionToken struct {
	SID  string
	CSRF string
}

type PiholeSync struct {
	config   Config
	logFile  *os.File
	password string
	verbose  bool
}

func (ps *PiholeSync) log(msg string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	logMsg := fmt.Sprintf("[%s] %s\n", timestamp, msg)
	fmt.Print(logMsg)
	ps.logFile.WriteString(logMsg)
}

func (ps *PiholeSync) debug(msg string) {
	if ps.verbose {
		ps.log(fmt.Sprintf("DEBUG: %s", msg))
	}
}

func (ps *PiholeSync) authenticate(host string, port int) (SessionToken, error) {
	ps.log(fmt.Sprintf("Authenticating to %s:%d...", host, port))

	authPayload := map[string]string{"password": ps.password}
	jsonPayload, _ := json.Marshal(authPayload)

	resp, err := http.Post(
		fmt.Sprintf("http://%s:%d/api/auth", host, port),
		"application/json",
		bytes.NewBuffer(jsonPayload),
	)
	if err != nil {
		ps.log(fmt.Sprintf("ERROR: Authentication request failed: %v", err))
		return SessionToken{}, err
	}
	defer resp.Body.Close()

	ps.debug(fmt.Sprintf("Auth response status: %d", resp.StatusCode))

	var authResp AuthResponse
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &authResp)

	ps.debug(fmt.Sprintf("Auth response: %s", string(body)))

	if authResp.Session.SID == nil || authResp.Session.CSRF == nil {
		return SessionToken{}, fmt.Errorf("authentication failed: %s", authResp.Session.Message)
	}

	ps.log(fmt.Sprintf("Authenticated to %s:%d successfully", host, port))
	return SessionToken{SID: *authResp.Session.SID, CSRF: *authResp.Session.CSRF}, nil
}

func (ps *PiholeSync) export(host string, port int, token SessionToken) ([]byte, error) {
	ps.log(fmt.Sprintf("Exporting config from %s:%d...", host, port))

	url := fmt.Sprintf("http://%s:%d/api/teleporter?action=export", host, port)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-CSRF-Token", token.CSRF)
	req.AddCookie(&http.Cookie{Name: "sid", Value: token.SID})

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		ps.log(fmt.Sprintf("ERROR: Export request failed: %v", err))
		return nil, err
	}
	defer resp.Body.Close()

	ps.debug(fmt.Sprintf("Export response status: %d", resp.StatusCode))

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		ps.log(fmt.Sprintf("ERROR: Export failed with status %d: %s", resp.StatusCode, string(respData)))
		return nil, fmt.Errorf("export failed with status %d", resp.StatusCode)
	}

	ps.debug(fmt.Sprintf("Exported data size: %d bytes", len(respData)))
	ps.log("Export successful")
	return respData, nil
}

func (ps *PiholeSync) importConfig(host string, port int, token SessionToken, data []byte) error {
	ps.log(fmt.Sprintf("Importing config to %s:%d...", host, port))

	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			wait := time.Duration(attempt*2) * time.Second
			ps.log(fmt.Sprintf("Retry %d/%d after %v delay...", attempt, maxRetries, wait))
			time.Sleep(wait)
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		writer.WriteField("action", "import")

		part, _ := writer.CreateFormFile("file", "pihole-teleport.zip")
		part.Write(data)
		writer.Close()

		req, _ := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/api/teleporter", host, port), body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("X-CSRF-Token", token.CSRF)
		req.AddCookie(&http.Cookie{Name: "sid", Value: token.SID})

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			ps.log(fmt.Sprintf("ERROR: Import request failed: %v", err))
			continue
		}
		defer resp.Body.Close()

		ps.debug(fmt.Sprintf("Import response status: %d", resp.StatusCode))

		respBody, _ := io.ReadAll(resp.Body)
		ps.debug(fmt.Sprintf("Import response: %s", string(respBody)))

		if resp.StatusCode == http.StatusOK {
			ps.log("Import successful")
			return nil
		}

		ps.log(fmt.Sprintf("Import attempt %d failed with status %d: %s", attempt, resp.StatusCode, string(respBody)))
	}

	return fmt.Errorf("import failed after %d attempts", maxRetries)
}

func (ps *PiholeSync) sync() error {
	ps.log("Starting Pi-hole Teleporter sync...")

	primaryToken, err := ps.authenticate(ps.config.Primary.Host, ps.config.Primary.Port)
	if err != nil {
		return fmt.Errorf("primary auth failed: %w", err)
	}

	secondaryToken, err := ps.authenticate(ps.config.Secondary.Host, ps.config.Secondary.Port)
	if err != nil {
		return fmt.Errorf("secondary auth failed: %w", err)
	}

	data, err := ps.export(ps.config.Primary.Host, ps.config.Primary.Port, primaryToken)
	if err != nil {
		return fmt.Errorf("export failed: %w", err)
	}

	if err := ps.importConfig(ps.config.Secondary.Host, ps.config.Secondary.Port, secondaryToken, data); err != nil {
		return fmt.Errorf("import failed: %w", err)
	}

	ps.log("Sync complete")
	return nil
}

func main() {
	configPath := flag.String("config", "", "Path to config.toml")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("Missing required -config argument")
	}

	var cfg Config
	if _, err := toml.DecodeFile(*configPath, &cfg); err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	password := os.Getenv("PIHOLE_ADMIN_PASSWORD")
	if password == "" {
		log.Fatal("PIHOLE_ADMIN_PASSWORD not set")
	}

	logFile, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()

	ps := &PiholeSync{
		config:   cfg,
		logFile:  logFile,
		password: password,
		verbose:  *verbose,
	}

	if err := ps.sync(); err != nil {
		ps.log(fmt.Sprintf("ERROR: %v", err))
		os.Exit(1)
	}
}
