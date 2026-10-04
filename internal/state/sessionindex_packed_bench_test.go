package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func BenchmarkPackedExactOwnerLookup(b *testing.B) {
	for _, physical := range []bool{false, true} {
		b.Run(fmt.Sprintf("physical-%v", physical), func(b *testing.B) {
			s, key, marker := packedOwnerFixture(b)
			shard := packedIndexHash(key)[:2]
			owners := map[agentmeta.SessionKey][]string{key: {"packed-owner"}}
			for i := 0; len(owners) < 390; i++ {
				next := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: fmt.Sprintf("packed-neighbor-%06d", i)}
				if packedIndexHash(next)[:2] != shard {
					continue
				}
				id := fmt.Sprintf("packed-neighbor-owner-%06d", i)
				data, err := json.Marshal(migrationRegistration(next, id))
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(s.registrationPath(id), data, 0600); err != nil {
					b.Fatal(err)
				}
				owners[next] = []string{id}
			}
			if err := s.recoverPackedShard(context.Background(), shard, marker, owners, nil); err != nil {
				b.Fatal(err)
			}
			if physical {
				if err := local.Write(qualifiedSessionIndexPath(s.home, key), indexEntry(key, "packed-owner")); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				id, found, err := s.ArchiveSessionID(key)
				if err != nil || !found || id != "packed-owner" {
					b.Fatalf("lookup=%q %v %v", id, found, err)
				}
			}
		})
	}
}
