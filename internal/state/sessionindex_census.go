package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Read-ahead is restricted to small registration documents. Larger documents
// use the existing serial reader, preserving support without multiplying their
// unbounded size by worker count. Results are consumed in directory order.
const recoveryReadAheadBytes = 64 * 1024
const recoveryReadAheadWorkers = 8

var errRecoveryReadAheadLarge = errors.New("registration exceeds bounded read-ahead")

type recoveryRegistrationRead struct {
	reg archive.SessionRegistration
	err error
}

func (s *Store) readRegistrationChunk(entries []os.DirEntry) []recoveryRegistrationRead {
	results := make([]recoveryRegistrationRead, len(entries))
	var workers sync.WaitGroup
	for i, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		workers.Add(1)
		go func(i int, entry os.DirEntry) {
			defer workers.Done()
			file, err := os.Open(s.registrationPath(strings.TrimSuffix(entry.Name(), ".json")))
			if err != nil {
				results[i].err = err
				return
			}
			var data bytes.Buffer
			_, err = data.ReadFrom(io.LimitReader(file, recoveryReadAheadBytes+1))
			_ = file.Close()
			if err != nil {
				results[i].err = err
				return
			}
			if data.Len() > recoveryReadAheadBytes {
				results[i].err = errRecoveryReadAheadLarge
				return
			}
			results[i].err = json.Unmarshal(data.Bytes(), &results[i].reg)
		}(i, entry)
	}
	workers.Wait()
	return results
}
