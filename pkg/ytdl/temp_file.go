package ytdl

import (
	"os"

	log "github.com/sirupsen/logrus"
)

func (f *TempFile) Close() error {
	err := f.File.Close()
	err1 := os.RemoveAll(f.dir)
	if err1 != nil {
		log.Errorf("could not remove temp dir: %v", err1)
	}
	return err
}

func (f *TempFile) Fullpath() string {
	return f.File.Name()
}
