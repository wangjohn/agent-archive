package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/credentials"
)

// The messages below name where an R2 key is kept, which depends on the
// platform: the Keychain on macOS, a private file elsewhere (see
// credentials.OpenDefault). Each takes the platform as a parameter, and its
// callers pass credentialGOOS, so both wordings are tested on any OS. macOS
// keeps the exact words it had before the file store existed; only the
// other platform's are new. A failure the store itself reports is worded by
// the error (credentials.RecoveryAction, storage.Diagnose), not here.

// openCredentialStoreError is the failure to open the credential store.
func openCredentialStoreError(goos string, err error) error {
	return fmt.Errorf("open %s: %w", credentials.StoreName(goos), err)
}

// storageOpenError is collect's and sync's failure to open the credential
// store for an R2 destination.
func storageOpenError(goos string, err error) error {
	if credentials.UsesKeychain(goos) {
		return fmt.Errorf("keychain unavailable: %w", err)
	}
	return fmt.Errorf("open the %s: %w", credentials.StoreName(goos), err)
}

// credentialCheckLabel is the name of setup's check that the credential
// store opens.
func credentialCheckLabel(goos string) string {
	name := credentials.StoreName(goos)
	return strings.ToUpper(name[:1]) + name[1:]
}

// credentialCheckFix says how to fix a credential store that cannot be
// opened: the release build hint is about the Keychain's cgo build alone, so
// it is not given elsewhere. locked is a locked Keychain.
func credentialCheckFix(goos string, locked bool) string {
	const s3 = "store in Amazon S3 with agent-archive setup --yes --provider s3."
	switch {
	case !credentials.UsesKeychain(goos):
		return "Fix what the message says, then run agent-archive setup again, or " + s3
	case locked:
		return "Unlock the login Keychain (log in, or open Keychain Access), then run agent-archive setup again, or " + s3
	}
	return "Use the release build of agent-archive, which can open the Keychain, or " + s3
}

// stagedCredentialLeftNote is what setup says of the credential a saved
// setup it moves aside may have staged.
func stagedCredentialLeftNote(goos, dataDir string) string {
	if credentials.UsesKeychain(goos) {
		return "A Keychain item it staged, if any, stays in the Keychain (service " + credentials.KeychainService + ")."
	}
	return "A credentials file it staged, if any, stays in " + credentials.FileStoreDir(dataDir) + "."
}

// unreadableDraftUninstallNote is what uninstall says when the saved setup
// cannot be read, so it cannot name a credential the setup staged. There is
// nothing to say where the credentials are files in the data directory, which
// uninstall deletes whole.
func unreadableDraftUninstallNote(goos, draftPath, problem string) string {
	if !credentials.UsesKeychain(goos) {
		return ""
	}
	return fmt.Sprintf("The saved setup in %s cannot be read (%s), so a Keychain item it staged, if any, is not deleted. Look for items of service %q in Keychain Access.", draftPath, problem, credentials.KeychainService)
}

// undeletedCredentialsProblem is uninstall's report of credentials it could
// not delete, with how to delete them yourself.
func undeletedCredentialsProblem(goos, dataDir string, undeleted []string, cause error) string {
	if !credentials.UsesKeychain(goos) {
		dir := credentials.FileStoreDir(dataDir)
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
