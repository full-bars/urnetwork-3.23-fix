// The local chain and the statement that awaits its signatures. Every write
// replaces a whole file atomically, with mode 0600. Writes from concurrent
// processes are not serialized; an operator refuses a generation lost that way
// as a fork.
package hotkeywallet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect/internal/durablesync"
)

const originalsFileName = "originals.json"
const pendingFileName = "pending.json"

// The JSON array of the longest chain the protocol admits, each original at
// its byte bound with a separator, and the brackets.
const maximumChainBytes = protocol.MaxWalletMappingHistory*(protocol.MaxWalletMappingConsentBytes+1) + 1

// Directory is <base>/hotkey-wallet. originals.json holds the chain as a
// protocol JSON array of originals, and pending.json the pending statement as
// its canonical protocol JSON.
type Store struct {
	Directory string
}

// The verified chain from generation 1, or nil before the first append.
func (self Store) Chain() ([]protocol.HotkeyWalletMappingConsent, error) {
	chain, err := self.readChain()
	if err != nil || chain == nil {
		return nil, err
	}
	if _, _, err := protocol.VerifyHotkeyWalletMappingLineage(context.Background(), chain); err != nil {
		return nil, fmt.Errorf("hotkey wallet chain %s: %w", self.path(originalsFileName), err)
	}
	return chain, nil
}

// Appends the next generation after verifying the complete chain with it. The
// identical original at a stored generation is already appended. A different
// one is a fork and is refused. A pending statement whose generation is now
// stored is removed.
func (self Store) Append(original protocol.HotkeyWalletMappingConsent) error {
	chain, err := self.readChain()
	if err != nil {
		return err
	}
	statement, err := protocol.DecodeHotkeyWalletMappingStatement(original.Message)
	if err != nil {
		return err
	}
	stored := 1 <= statement.Generation && statement.Generation <= uint64(len(chain))
	if stored && chain[statement.Generation-1] != original {
		return fmt.Errorf("%w: generation %d is already stored with a different original", protocol.ErrWalletMappingIntegrity, statement.Generation)
	}
	if !stored {
		chain = append(chain, original)
	}
	if _, _, err := protocol.VerifyHotkeyWalletMappingLineage(context.Background(), chain); err != nil {
		return err
	}
	if !stored {
		raw, err := json.Marshal(chain)
		if err != nil {
			return err
		}
		if err := writeFile(self.path(originalsFileName), raw); err != nil {
			return err
		}
	}
	// a pending statement for a stored generation can never be appended; an
	// unreadable one is left for SetPending to replace
	if pending, err := self.Pending(); err == nil && pending != nil && pending.Generation <= uint64(len(chain)) {
		if err := os.Remove(self.path(pendingFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Replaces the pending statement. The statement must be valid.
func (self Store) SetPending(statement protocol.HotkeyWalletMappingStatement) error {
	message, err := statement.Message()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(message, protocol.HotkeyWalletMappingConsentPrefix) {
		return protocol.ErrWalletMappingIntegrity
	}
	return writeFile(self.path(pendingFileName), []byte(strings.TrimPrefix(message, protocol.HotkeyWalletMappingConsentPrefix)))
}

// The statement that awaits its signatures, or nil when none does. It decodes
// exactly as its signed message does.
func (self Store) Pending() (*protocol.HotkeyWalletMappingStatement, error) {
	raw, err := readFile(self.path(pendingFileName), protocol.MaxWalletMappingMessageBytes)
	if err != nil || raw == nil {
		return nil, err
	}
	statement, err := protocol.DecodeHotkeyWalletMappingStatement(protocol.HotkeyWalletMappingConsentPrefix + string(raw))
	if err != nil {
		return nil, fmt.Errorf("pending hotkey wallet statement %s: %w", self.path(pendingFileName), err)
	}
	return statement, nil
}

func (self Store) path(name string) string {
	return filepath.Join(self.Directory, name)
}

// The stored chain, unverified, or nil when none is stored.
func (self Store) readChain() ([]protocol.HotkeyWalletMappingConsent, error) {
	path := self.path(originalsFileName)
	raw, err := readFile(path, maximumChainBytes)
	if err != nil || raw == nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var chain []protocol.HotkeyWalletMappingConsent
	if err := decoder.Decode(&chain); err != nil {
		return nil, fmt.Errorf("hotkey wallet chain %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("hotkey wallet chain %s holds more than one JSON value", path)
	}
	if len(chain) == 0 || len(chain) > protocol.MaxWalletMappingHistory {
		return nil, fmt.Errorf("hotkey wallet chain %s must hold 1 to %d originals", path, protocol.MaxWalletMappingHistory)
	}
	return chain, nil
}

// A missing file is (nil, nil); a file larger than maximum is an error.
func readFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return raw, nil
}

// Writes a private temporary beside the file, syncs it and renames it over the
// file, so a crash leaves either the old or the new complete file.
func writeFile(path string, raw []byte) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if returnErr != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return durablesync.SyncDir(directory)
}
