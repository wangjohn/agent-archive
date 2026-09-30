package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// The messages below name where an R2 key is kept, which depends on the
// platform: the Keychain on macOS, a private file elsewhere (see
// credentials.OpenDefault). Each takes the platform as a parameter, and its
// callers pass credentialOS, so both wordings are tested on any OS. macOS
// keeps the exact words it had before the file store existed; only the
// other platform's are new. A failure the store itself reports is worded by
// the error (credentials.RecoveryAction, storage.Diagnose), not here.
//
// An Unknown platform never has a store to describe: credentials.OpenDefault
// refuses it (ErrUnsupportedPlatform). So the words that name the store call
// it "credential store", and the rest, which describe what a Keychain is not,
// give the file store's wording without ever claiming the Keychain.

// openCredentialStoreError is the failure to open the credential store.
func openCredentialStoreError(system platform.OS, err error) error {
	return fmt.Errorf("open %s: %w", credentials.StoreName(system), err)
}

// storageOpenError is collect's and sync's failure to open the credential
// store for an R2 destination.
func storageOpenError(system platform.OS, err error) error {
	if credentials.UsesKeychain(system) {
		return fmt.Errorf("keychain unavailable: %w", err)
	}
	return fmt.Errorf("open the %s: %w", credentials.StoreName(system), err)
}

// credentialCheckLabel is the name of setup's check that the credential
// store opens.
func credentialCheckLabel(system platform.OS) string {
	name := credentials.StoreName(system)
	return strings.ToUpper(name[:1]) + name[1:]
}

// credentialCheckFix says how to fix a credential store that cannot be
// opened: the release build hint is about the Keychain's cgo build alone, so
// it is not given elsewhere. locked is a locked Keychain.
func credentialCheckFix(system platform.OS, locked bool) string {
	const s3 = "store in Amazon S3 with agent-archive setup --yes --provider s3."
	switch {
	case !credentials.UsesKeychain(system):
		return "Fix what the message says, then run agent-archive setup again, or " + s3
	case locked:
		return "Unlock the login Keychain (log in, or open Keychain Access), then run agent-archive setup again, or " + s3
	}
	return "Use the release build of agent-archive, which can open the Keychain, or " + s3
}

// stagedCredentialLeftNote is what setup says of the credential a saved
// setup it moves aside may have staged.
func stagedCredentialLeftNote(system platform.OS, dataDir string) string {
	if credentials.UsesKeychain(system) {
		return "A Keychain item it staged, if any, stays in the Keychain (service " + credentials.KeychainService + ")."
	}
	return "A credentials file it staged, if any, stays in " + credentials.FileStoreDir(dataDir) + "."
}

// unreadableDraftUninstallNote is what uninstall says when the saved setup
// cannot be read, so it cannot name a credential the setup staged. There is
// nothing to say where the credentials are files in the data directory, which
// uninstall deletes whole.
func unreadableDraftUninstallNote(system platform.OS, draftPath, problem string) string {
	if !credentials.UsesKeychain(system) {
		return ""
	}
	return fmt.Sprintf("The saved setup in %s cannot be read (%s), so a Keychain item it staged, if any, is not deleted. Look for items of service %q in Keychain Access.", draftPath, problem, credentials.KeychainService)
}

// credentialFolder is what uninstall knows about the credentials folder
// (credentials.FileStoreDir) when it reports credentials it could not delete
// one by one: whether the folder was a link before the purge (and to where),
// and whether anything is still at its path after it.
type credentialFolder struct {
	// isLink is whether the folder was a symbolic link, and linkTarget where
	// to.
	isLink     bool
	linkTarget string
	// remains is whether anything is at the folder's path after the purge.
	remains bool
}

// lookCredentialFolder reads the folder's link state; call it before the
// purge, with dataDir the data directory.
func lookCredentialFolder(dataDir string) credentialFolder {
	dir := credentials.FileStoreDir(dataDir)
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return credentialFolder{}
	}
	target, _ := os.Readlink(dir)
	if target != "" && !filepath.IsAbs(target) {
		// A relative link is relative to the folder that holds the link.
		target = filepath.Join(filepath.Dir(dir), target)
	}
	return credentialFolder{isLink: true, linkTarget: target}
}

// afterPurge is f with whether the folder remains, read after the purge.
func (f credentialFolder) afterPurge(dataDir string) credentialFolder {
	_, err := os.Lstat(credentials.FileStoreDir(dataDir))
	f.remains = err == nil
	return f
}

// undeletedCredentialsProblem is uninstall's report of credentials it could
// not delete, with how to delete them yourself. It is "" when there is
// nothing left to report: off macOS the credentials are files in the
// credentials folder, which the purge removes whole, so credentials that
// could not be deleted one by one are gone with it, unless the folder was a
// link (the purge removes the link, never what it points to) or is still
// there.
func undeletedCredentialsProblem(system platform.OS, dataDir string, undeleted []string, cause error, folder credentialFolder) string {
	if !credentials.UsesKeychain(system) {
		dir := credentials.FileStoreDir(dataDir)
		switch {
		case folder.isLink:
			return fmt.Sprintf("%d stored credential(s) could not be deleted: %v. The credentials folder %s is a link to %s, which uninstall does not follow. To remove them yourself, delete the files in %s", len(undeleted), cause, dir, folder.linkTarget, folder.linkTarget)
		case !folder.remains:
			return ""
		}
		return fmt.Sprintf("%d stored credential(s) could not be deleted from the credentials folder %s: %v. To remove them yourself, delete the files in that folder", len(undeleted), dir, cause)
	}
	commands := make([]string, 0, len(undeleted))
	for _, ref := range undeleted {
		commands = append(commands, fmt.Sprintf("security delete-generic-password -s %s -a %s", credentials.KeychainService, ref))
	}
	problem := fmt.Sprintf("%d stored credential(s) could not be deleted from Keychain service %q: %v", len(undeleted), credentials.KeychainService, cause)
	if errors.Is(cause, credentials.ErrKeychainLocked) {
		problem += ". Unlock the login Keychain (log in, or open Keychain Access)"
	}
	return problem + fmt.Sprintf(". To remove them yourself, run: %s; or delete those items in Keychain Access", strings.Join(commands, " && "))
}
