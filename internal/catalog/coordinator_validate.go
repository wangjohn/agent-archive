package catalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/storage"
	"io"
)

func exactHex(value string, bytes int) bool {
	if len(value) != bytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (state admissions) validate() error {
	invalid := errors.New("invalid catalog coordinator descriptor")
	if state.Protocol != 9 || state.Generation == 0 || state.Owners == nil || len(state.Owners) > 256 {
		return invalid
	}
	if state.Mode != "candidate" && state.Mode != "active" && state.Mode != "rollback" {
		return invalid
	}
	if state.Mode == "active" && state.Proof == "" {
		return invalid
	}
	if state.Seal != "" && !exactHex(state.Seal, 24) {
		return invalid
	}
	if state.Hold != "" && (state.Seal == "" || !exactHex(state.Hold, 24) || len(state.Owners) != 0) {
		return invalid
	}
	for owner, claim := range state.Owners {
		if owner == "" || len(owner) > 512 || claim.Digest == "" || len(claim.Refs) > 256 {
			return invalid
		}
		for _, ref := range claim.Refs {
			if ref.Key == "" || ref.SHA256 == "" || len(ref.Key) > 4096 {
				return invalid
			}
		}
	}
	if receipt := state.GCReceipt; receipt != nil && (!exactHex(receipt.Owner, 24) || !exactHex(receipt.ReleasedSHA256, 32)) {
		return invalid
	}
	link := state.GCLink
	if link == nil {
		return nil
	}
	if !exactHex(link.Owner, 24) || link.StateGeneration != state.Generation || link.Witness.Generation == 0 || link.Witness.Generation >= state.Generation || link.Witness.Seal != state.Seal || link.Witness.Hold != state.Hold || state.Hold == "" || len(state.Owners) != 0 || link.PriorETag == "" || len(link.PriorETag) > 4096 || !exactHex(link.PriorSHA256, 32) || !exactHex(link.LeasedSHA256, 32) || inventoryHash(link.Inventory) != link.Witness.InventorySHA256 {
		return invalid
	}
	if link.Phase != "prepared" && link.Phase != "releasing" {
		return invalid
	}
	if link.Phase == "prepared" && (link.ReleasedSHA256 != "" || len(link.ReleasedHead) != 0) {
		return invalid
	}
	if link.Phase == "releasing" {
		if !exactHex(link.ReleasedSHA256, 32) || len(link.ReleasedHead) == 0 || len(link.ReleasedHead) > 16<<10 || !exactHex(link.Witness.InventorySHA256, 32) {
			return invalid
		}
		if storage.SHA256Hex(link.ReleasedHead) != link.ReleasedSHA256 {
			return invalid
		}
		var h CatalogHead
		decoder := json.NewDecoder(bytes.NewReader(link.ReleasedHead))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&h) != nil || decoder.Decode(new(any)) != io.EOF || h.GCLease != "" || h.GCCoordinator != (gcCoordinatorWitness{}) || h.Protocol != 9 || h.Schema != 4 || h.Generation == 0 || !exactHex(h.Epoch, 24) || h.validateReferences() != nil {
			return invalid
		}
	}
	return nil
}
