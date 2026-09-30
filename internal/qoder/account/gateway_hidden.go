package account

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func gatewayHiddenPath(id string) string {
	return filepath.Join(DataRoot(), "work2api_hidden_accounts", fmt.Sprintf("%x", sha256.Sum256([]byte(id))))
}

func IsGatewayHidden(id string) bool {
	_, err := os.Stat(gatewayHiddenPath(id))
	return err == nil || !os.IsNotExist(err)
}

func SetGatewayHidden(id string, hidden bool) error {
	path := gatewayHiddenPath(id)
	if !hidden {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("hidden"), 0600)
}
