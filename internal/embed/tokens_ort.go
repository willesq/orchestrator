//go:build ORT

package embed

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/daulet/tokenizers"
)

// loadTokenCounter loads the model's tokenizer for counting. Its truncation
// and padding are removed first: the model's tokenizer.json stops at 128
// tokens and pads every input to a fixed length, and a counter doing either
// cannot tell a chunk that fits from one that is cut off. It returns nil, and callers estimate, if the tokenizer cannot be
// loaded.
func loadTokenCounter(modelDir string) tokenCounter {
	raw, err := os.ReadFile(filepath.Join(modelDir, "tokenizer.json"))
	if err != nil {
		log.Printf("embed: token counting falls back to an estimate: %v", err)
		return nil
	}
	// Raw values, so nothing but these two settings changes on the way through.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		log.Printf("embed: token counting falls back to an estimate: tokenizer.json is not an object")
		return nil
	}
	doc["truncation"] = json.RawMessage("null")
	doc["padding"] = json.RawMessage("null")
	untruncated, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	tk, err := tokenizers.FromBytes(untruncated)
	if err != nil {
		log.Printf("embed: token counting falls back to an estimate: %v", err)
		return nil
	}
	var mu sync.Mutex
	return func(text string) int {
		mu.Lock()
		defer mu.Unlock()
		ids, _ := tk.Encode(text, false)
		return len(ids)
	}
}
