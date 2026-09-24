package hooks

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Change is prepared before any mutation so callers can show a concrete plan.
type Change struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    os.FileMode
}

func Plan(home, executable string, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		relative, err := hookFile(h)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(home, relative)
		before, err := os.ReadFile(path)
		exists := err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, errors.New("cannot read existing hook configuration")
		}
		after, err := Merge(before, h, executable)
		if err != nil {
			return nil, err
		}
		mode := os.FileMode(0600)
		if exists {
			info, e := os.Stat(path)
			if e != nil {
				return nil, e
			}
			mode = info.Mode().Perm()
		}
		changes = append(changes, Change{path, before, after, exists, mode})
	}
	return changes, nil
}

// Apply rolls back already written files on failure. It refuses a configuration
// changed since the plan was prepared, rather than overwriting concurrent edits.
func Apply(changes []Change) error {
	applied := []Change{}
	for _, c := range changes {
		current, err := os.ReadFile(c.Path)
		exists := err == nil
		if (err != nil && !os.IsNotExist(err)) || exists != c.Existed || string(current) != string(c.Before) {
			return errors.Join(errors.New("hook configuration changed during setup; retry setup"), rollback(applied))
		}
		if err = atomicWrite(c.Path, c.After, c.Mode); err != nil {
			return errors.Join(errors.New("cannot install hooks"), rollback(applied))
		}
		applied = append(applied, c)
	}
	return nil
}

// PlanRemoval prepares the inverse of Plan for uninstall: for each harness,
// a Change whose After is the current file with only our own handlers
// stripped (see Remove). A harness whose hook file is missing, or whose file
// never contained our entries, yields no Change at all, so an unrelated
// configuration is never rewritten or reformatted. Apply the result with
// Apply, which keeps its refuse-on-concurrent-edit and rollback behavior.
func PlanRemoval(home string, harnesses []string) ([]Change, error) {
	changes := []Change{}
	for _, h := range harnesses {
		relative, err := hookFile(h)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(home, relative)
		before, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, errors.New("cannot read existing hook configuration")
		}
		after, removed, err := Remove(before, h)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", relative, err)
		}
		if !removed {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, Change{path, before, after, true, info.Mode().Perm()})
	}
	return changes, nil
}

// hookFile is the per-harness hook configuration file, relative to the
// user's home directory.
func hookFile(harness string) (string, error) {
	switch harnessName(harness) {
	case harnessCodex:
		return ".codex/hooks.json", nil
	case harnessClaude:
		return ".claude/settings.json", nil
	case harnessCursor:
		return ".cursor/hooks.json", nil
	default:
		return "", errors.New("unsupported harness")
	}
}

func Rollback(changes []Change) error { return rollback(changes) }

func rollback(changes []Change) error {
	var failures []error
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		current, e := os.ReadFile(c.Path)
		if e != nil || string(current) != string(c.After) {
			failures = append(failures, errors.New("hook configuration changed; manual recovery required"))
			continue
		}
		if c.Existed {
			e = atomicWrite(c.Path, c.Before, c.Mode)
		} else {
			e = os.Remove(c.Path)
		}
		if e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-")
	if err != nil {
		return err
	}
	name := f.Name()
	// After a successful rename there is nothing left at name to remove.
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

const LaunchLabel = "com.agent-archive.collector"

func LaunchAgent(executable, dataHome string) ([]byte, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(dataHome) {
		return nil, errors.New("LaunchAgent paths must be absolute")
	}
	escape := func(s string) string {
		var b strings.Builder
		// A strings.Builder never fails a write.
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>_collect</string></array>
<key>EnvironmentVariables</key><dict><key>AGENT_ARCHIVE_HOME</key><string>%s</string></dict>
<key>RunAtLoad</key><true/><key>StartInterval</key><integer>60</integer>
<key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, LaunchLabel, escape(executable), escape(dataHome), escape(filepath.Join(dataHome, "collector.log")), escape(filepath.Join(dataHome, "collector-error.log")))), nil
}

// LaunchAgentProgram returns the executable a LaunchAgent plist runs: the
// first ProgramArguments string. It reads any well-formed plist, not only one
// LaunchAgent wrote, and reports an error when there is no program to read.
func LaunchAgentProgram(plist []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = false
	var (
		lastKey      string
		inArguments  bool
		readingKey   bool
		readingValue bool
		text         strings.Builder
	)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return "", errors.New("LaunchAgent has no ProgramArguments")
		}
		if err != nil {
			return "", fmt.Errorf("read LaunchAgent: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = true
				text.Reset()
			case "array":
				inArguments = lastKey == "ProgramArguments"
			case "string":
				readingValue = inArguments
				text.Reset()
			}
		case xml.CharData:
			if readingKey || readingValue {
				text.Write(t)
			}
		case xml.EndElement:
			//lint:ignore LV1001 plist element names come from an external XML format; any other element is skipped
			switch t.Name.Local {
			case "key":
				readingKey = false
				lastKey = strings.TrimSpace(text.String())
			case "string":
				if readingValue {
					if program := strings.TrimSpace(text.String()); program != "" {
						return program, nil
					}
					return "", errors.New("LaunchAgent program is empty")
				}
			case "array":
				if inArguments {
					return "", errors.New("LaunchAgent ProgramArguments is empty")
				}
			}
		}
	}
}

// Installed checks the complete expected configuration without changing it.
// JSON formatting and object key order do not affect the result.
func Installed(home, executable, harness string) (bool, error) {
	changes, err := Plan(home, executable, []string{harness})
	if err != nil {
		return false, err
	}
	for _, c := range changes {
		if !c.Existed {
			return false, nil
		}
		var before, after any
		if err := json.Unmarshal(c.Before, &before); err != nil {
			return false, err
		}
		if err := json.Unmarshal(c.After, &after); err != nil {
			return false, err
		}
		if !reflect.DeepEqual(before, after) {
			return false, nil
		}
	}
	return true, nil
}
