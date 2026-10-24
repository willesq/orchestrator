package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/willesq/orchestrator/internal/logs"
	orchdv1 "github.com/willesq/orchestrator/internal/rpc/orchdv1"
)

func newRAGShowCmd(cf clientFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "show <source-uri>",
		Short: "Print one source rebuilt from its stored chunks",
		Long: `Print a source as the vector store holds it: its chunks in order, without
their section context lines, with the overlap between chunks trimmed. This is
what retrieval sees; it is close to, but not exactly, the original text.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, cleanup, err := cf()
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			doc, err := client.GetDocument(ctx, &orchdv1.GetDocumentRequest{SourceUri: args[0]})
			if err != nil {
				return err
			}
			out, errOut := c.OutOrStdout(), c.ErrOrStderr()
			// Header and notes go to stderr, so stdout is the document alone.
			fmt.Fprintf(errOut, "%s (%s, %d chunks)\n", logs.Clean(doc.GetSourceUri()), logs.Clean(doc.GetSourceType()), doc.GetChunks())
			if u := doc.GetWebUrl(); u != "" {
				fmt.Fprintf(errOut, "original: %s\n", logs.Clean(u))
			}
			if doc.GetMalformed() {
				fmt.Fprintln(errOut, "note: chunk order is incomplete; the text is best effort")
			}
			if doc.GetOversize() {
				fmt.Fprintln(errOut, "note: too large to rebuild here")
				return nil
			}
			fmt.Fprintln(out, printableText(doc.GetText()))
			return nil
		},
	}
}

// printableText keeps a stored document's lines and tabs but replaces every
// other control and format character: indexed text comes from web pages,
// third-party repos and agents, and must not drive the terminal (escape
// sequences, hyperlinks, clipboard writes, bidi reordering).
func printableText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '\uFFFD'
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}
