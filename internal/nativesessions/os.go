package nativesessions

import (
	"io/fs"
	"os"
)

func readDirectory(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
