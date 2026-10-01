package cli

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The storage menu grows as guided choices are added (S3 creation, R2
// creation), so a test that types its numbers, or expects "Enter 1-N", must
// take them from storageMenuOptions() and not from the count of the day.

// storageMenuNumber is the number that chooses the storage menu's entry key,
// as a person types it.
func storageMenuNumber(t *testing.T, key string) string {
	t.Helper()
	for i, o := range storageMenuOptions() {
		if o.Key == key {
			return strconv.Itoa(i + 1)
		}
	}
	return key
}

// storageMenuPromptS3 is the answer line of the storage menu when Amazon S3
// is its default entry.
func storageMenuPromptS3() string { return "Choose [2]: " }

func TestStorageMenuNumbersFollowTheMenu(t *testing.T) {
	t.Parallel()
	if got := storageMenuNumber(t, "r2"); got != "1" {
		t.Fatal(got)
	}
	if got := storageMenuNumber(t, "s3"); got != "2" {
		t.Fatal(got)
	}
	if len(storageMenuOptions()) != 2 {
		t.Fatal("provider menu must contain two choices")
	}
}

// TestStorageMenuNumbersAreNotHardCoded fails when a test that drives the
// storage menu spells out the menu's size or an entry's number.
func TestStorageMenuNumbersAreNotHardCoded(t *testing.T) {
	t.Parallel()
	for file, patterns := range map[string][]*regexp.Regexp{
		// A quoted "Enter 1-N " (with a space after N) is the storage menu's
		// answer line; the bucket and profile pickers write "Enter 1-N, or
		// another".
		"setup_aws_test.go":       {regexp.MustCompile(`"Enter 1-[0-9]+ `)},
		"setup_s3_create_test.go": {regexp.MustCompile(`"Enter 1-[0-9]+ `)},
		// Choosing the instructions, or a guided entry, by number.
		"setup_prompts_test.go": {regexp.MustCompile(`"help\\n[0-9]`)},
		"setup_trims_test.go":   {regexp.MustCompile(`"help\\n[0-9]`), regexp.MustCompile(`\{"", "[0-9]+", "2"`)},
		// Guided R2 creation's tests choose their entry by key.
		"setup_r2_create_test.go": {regexp.MustCompile(`"Enter 1-[0-9]+ `), regexp.MustCompile(`"help\\n[0-9]`), regexp.MustCompile(`\{"", "[0-9]+", "2"`)},
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range patterns {
			if loc := pattern.FindIndex(data); loc != nil {
				t.Errorf("%s spells out a storage menu number (%q): take it from storageMenuNumber or storageMenuPromptS3", file, data[loc[0]:loc[1]])
			}
		}
	}
}
