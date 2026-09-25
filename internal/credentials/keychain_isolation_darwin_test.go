//go:build darwin && cgo

package credentials

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// isolateKeychainForTesting replaces every Keychain call KeychainStore makes
// with one that panics, naming the call, so a test that reaches the Keychain
// stops instead of reading, writing, or deleting a real item.
func isolateKeychainForTesting() {
	keychain = keychainAccess{
		get: func(service, account string) ([]byte, int) {
			panic(fmt.Sprintf("a test reached the real Keychain: read %s/%s", service, account))
		},
		save: func(service, account string, _ []byte) int {
			panic(fmt.Sprintf("a test reached the real Keychain: save %s/%s", service, account))
		},
		delete: func(service, account string) int {
			panic(fmt.Sprintf("a test reached the real Keychain: delete %s/%s", service, account))
		},
	}
}

// Every KeychainStore call that gets past its argument checks goes through
// the keychain seam, which TestMain made fail closed: each one stops the
// test here. A call that bypassed the seam would reach the real Keychain
// instead and fail this test by not panicking.
func TestKeychainStoreFailsClosedInTests(t *testing.T) {
	// Checked from the source first: a call that bypassed the seam would
	// otherwise reach the real Keychain below.
	if outside := keychainCallsOutsideSeam(t); len(outside) > 0 {
		t.Fatalf("Keychain calls outside securityFramework: %v", outside)
	}
	store, err := NewKeychainStore("agent-archive-test-never-used")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func(){
		"read":   func() { _, _ = store.Load(ctx, "ref") },
		"save":   func() { _ = store.Save(ctx, "ref", R2Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}) },
		"delete": func() { _ = store.Delete(ctx, "ref") },
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "a test reached the real Keychain: "+name) {
					t.Errorf("%s: got %v, want the fail-closed stand-in to stop it", name, r)
				}
			}()
			call()
		}()
	}
}

// keychainCallsOutsideSeam lists the calls into the Keychain's C functions
// (C.aa_keychain_*) in keychain_darwin.go that are not inside
// securityFramework, the one value tests replace.
func keychainCallsOutsideSeam(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "keychain_darwin.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seam ast.Node
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			for _, spec := range gen.Specs {
				if value, ok := spec.(*ast.ValueSpec); ok && len(value.Names) == 1 && value.Names[0].Name == "securityFramework" {
					seam = value
				}
			}
		}
	}
	if seam == nil {
		t.Fatal("securityFramework not found in keychain_darwin.go")
	}
	var outside []string
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "C" || !strings.HasPrefix(sel.Sel.Name, "aa_keychain_") {
			return true
		}
		calls++
		if sel.Pos() < seam.Pos() || sel.End() > seam.End() {
			outside = append(outside, fmt.Sprintf("%s at %s", sel.Sel.Name, fset.Position(sel.Pos())))
		}
		return true
	})
	if calls == 0 {
		t.Fatal("no C.aa_keychain_ call found in keychain_darwin.go")
	}
	return outside
}
