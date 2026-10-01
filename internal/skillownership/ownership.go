// Package skillownership holds pure shared managed skill ownership rules.
package skillownership

import (
	"bytes"
	"strings"
)

// Marker identifies files written by shared archive setup templates.
const Marker = "<!-- Written by agent-archive setup, which replaces this file; agent-archive uninstall removes it. Delete this line to keep your own version. -->"

// Owned requires the exact setup marker and installation data-home convention.
func Owned(content []byte, dataHome string) bool {
	found := false
	for _, line := range strings.Split(string(content), "\n") {
		if line == Marker {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if dataHome == "" {
		return !bytes.Contains(content, []byte("AGENT_ARCHIVE_HOME="))
	}
	return bytes.Contains(content, []byte("AGENT_ARCHIVE_HOME="+quote(dataHome)+" "))
}
func quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
