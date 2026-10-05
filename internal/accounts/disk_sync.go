package accounts

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncCredentialToDisk writes the latest tokens back to CPA auth files
// in data/cpa_auth_files/ so local disk credentials remain strictly in sync
// with PostgreSQL under single-system sovereign management.
func SyncCredentialToDisk(entry map[string]any) error {
	if entry == nil {
		return nil
	}
	email := strings.TrimSpace(stringField(entry, "email"))
	if email == "" {
		return nil
	}
	access := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"))
	rt := stringField(entry, "refresh_token")
	if access == "" && rt == "" {
		return nil
	}

	dataDirs := []string{}
	if envDir := strings.TrimSpace(os.Getenv("GROK2API_DATA_DIR")); envDir != "" {
		dataDirs = append(dataDirs, envDir)
	}
	dataDirs = append(dataDirs, "/app/data", "./data", "../data")

	var targetDir string
	for _, dir := range dataDirs {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			targetDir = filepath.Join(dir, "cpa_auth_files")
			break
		}
	}
	if targetDir == "" {
		targetDir = filepath.Join("./data", "cpa_auth_files")
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	// Candidates for this email
	candidates := []string{
		filepath.Join(targetDir, fmt.Sprintf("codex-%s-free.json", email)),
		filepath.Join(targetDir, fmt.Sprintf("codex-%s.json", email)),
		filepath.Join(targetDir, fmt.Sprintf("chatgpt-%s.json", email)),
	}

	var matchedFile string
	for _, cand := range candidates {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			matchedFile = cand
			break
		}
	}

	// Fallback to default CPA codex filename if none exists
	if matchedFile == "" {
		matchedFile = filepath.Join(targetDir, fmt.Sprintf("codex-%s-free.json", email))
	}

	doc := map[string]any{}
	if data, err := os.ReadFile(matchedFile); err == nil {
		_ = json.Unmarshal(data, &doc)
	}

	if access != "" {
		doc["access_token"] = access
	}
	if rt != "" {
		doc["refresh_token"] = rt
	}
	if idt := stringField(entry, "id_token"); idt != "" {
		doc["id_token"] = idt
	}
	doc["email"] = email
	doc["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	if _, ok := doc["type"]; !ok {
		doc["type"] = "codex"
	}

	if expFloat := ParseExpiresAt(entry["expires_at"], access); expFloat != nil && *expFloat > 0 {
		expTime := time.Unix(int64(*expFloat), 0).UTC()
		doc["expired"] = expTime.Format(time.RFC3339)
	}

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmpFile := fmt.Sprintf("%s.tmp.%d", matchedFile, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, raw, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmpFile, matchedFile); err != nil {
		_ = os.Remove(tmpFile)
		return err
	}

	slog.Info("sovereign maintainer synced credentials to disk file", "email", email, "file", filepath.Base(matchedFile))
	return nil
}
