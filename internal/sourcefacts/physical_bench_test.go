package sourcefacts

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkPhysicalProjects measures distinct project work independently of
// source-file/registration counts, including aggregate-budget scheduling slices.
func BenchmarkPhysicalProjects(b *testing.B) {
	for _, kind := range []string{"git", "nongit"} {
		for _, count := range []int{1, 100, 1000, 10000} {
			b.Run(fmt.Sprintf("%s/%d", kind, count), func(b *testing.B) {
				base := b.TempDir()
				cwds := make([]string, count)
				for i := range cwds {
					cwds[i] = filepath.Join(base, fmt.Sprintf("group-%d", i%100), fmt.Sprintf("project-%d", i))
					path := cwds[i]
					if kind == "git" {
						path = filepath.Join(path, ".git")
					}
					if e := os.MkdirAll(path, 0700); e != nil {
						b.Fatal(e)
					}
				}
				b.ResetTimer()
				b.ReportAllocs()
				operations, slices := 0, 0
				for range b.N {
					resolver := NewProjectResolver()
					slices++
					for _, cwd := range cwds {
						_, ok := resolver.Resolve(cwd)
						if !ok && resolver.Exhausted {
							operations += resolver.Operations
							resolver = NewProjectResolver()
							slices++
							_, ok = resolver.Resolve(cwd)
						}
						if !ok {
							b.Fatal("physical identity unresolved")
						}
					}
					operations += resolver.Operations
				}
				b.ReportMetric(float64(operations)/float64(b.N), "metadata_ops/run")
				b.ReportMetric(float64(slices)/float64(b.N), "slices/run")
			})
		}
	}
}
