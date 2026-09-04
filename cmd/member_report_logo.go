package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// memberReportLogoPath is where the hub's logo lives once fetched. PNG because
// that is what Odoo stores and what fpdf embeds without conversion.
func memberReportLogoPath() string {
	return settingsFilePath("logo.png")
}

// FetchCompanyLogoFromOdoo copies the company logo out of Odoo's res.company
// record into settings/logo.png.
//
// Strictly read-only: a single search_read, no writes. It is a separate,
// explicit step rather than part of `chb odoo pull` because the logo changes
// roughly never, and a routine fetch should not carry an image download.
func FetchCompanyLogoFromOdoo() (string, error) {
	creds, err := ResolveOdooCredentials()
	if err != nil {
		return "", err
	}
	uid, err := odooAuth(creds.URL, creds.DB, creds.Login, creds.Password)
	if err != nil || uid == 0 {
		return "", wrapOdooAuthError(err)
	}

	raw, err := odooExec(creds.URL, creds.DB, uid, creds.Password,
		"res.company", "search_read",
		[]interface{}{[]interface{}{}},
		map[string]interface{}{"fields": []string{"id", "name", "logo"}, "limit": 1})
	if err != nil {
		return "", fmt.Errorf("reading res.company: %w", err)
	}

	var rows []struct {
		Name string `json:"name"`
		Logo any    `json:"logo"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", fmt.Errorf("decoding res.company: %w", err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no company record found in Odoo")
	}
	encoded, ok := rows[0].Logo.(string)
	if !ok || strings.TrimSpace(encoded) == "" {
		return "", fmt.Errorf("company %q has no logo set in Odoo", rows[0].Name)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding logo image: %w", err)
	}
	if !isPNG(data) {
		return "", fmt.Errorf("company logo is not a PNG (%d bytes) — save it as PNG in Odoo, or drop one at %s",
			len(data), memberReportLogoPath())
	}
	path := memberReportLogoPath()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// isPNG checks the 8-byte PNG signature. fpdf infers the image type from the
// bytes, so handing it something else fails deep inside the renderer with a
// far less useful message.
func isPNG(data []byte) bool {
	sig := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if len(data) < len(sig) {
		return false
	}
	for i, b := range sig {
		if data[i] != b {
			return false
		}
	}
	return true
}

// resolveLogoPath returns the logo to draw, or "" when there is none. An
// explicit --logo wins; otherwise the cached settings copy is used.
func resolveLogoPath(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	if p := memberReportLogoPath(); fileExists(p) {
		return p
	}
	return ""
}
