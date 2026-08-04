package rag

import "strings"

// chunkText splits text into overlapping chunks of approximately chunkSize
// whitespace tokens with overlap tokens shared between neighbours.
//
// This is a deliberately simple word-based approximation. Chunk boundaries need
// not match the Python system byte-for-byte: the Go rewrite re-embeds the index
// from scratch, so exact tokenizer-based boundaries (tiktoken) are a later refinement.
func chunkText(text string, chunkSize, overlap int) []string {
	if chunkSize <= 0 {
		chunkSize = 400
	}
	if overlap < 0 || overlap >= chunkSize {
		overlap = 0
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	if len(fields) <= chunkSize {
		return []string{strings.Join(fields, " ")}
	}
	step := chunkSize - overlap
	var chunks []string
	for start := 0; start < len(fields); start += step {
		end := start + chunkSize
		if end > len(fields) {
			end = len(fields)
		}
		chunks = append(chunks, strings.Join(fields[start:end], " "))
		if end == len(fields) {
			break
		}
	}
	return chunks
}
