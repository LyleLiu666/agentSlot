package agentslot_test

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/LyleLiu666/agentSlot/componentcatalog"
	"github.com/LyleLiu666/agentSlot/loop"
)

var (
	componentMapRowPattern      = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9.-]*)` \\|")
	englishContractedRowPattern = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9.-]*)` \\|.*\\| Contracted \\|$")
	chineseContractedRowPattern = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9.-]*)` \\|.*\\| 已定义契约 \\|$")
	englishConformantRowPattern = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9.-]*)` \\|.*\\| Conformant \\|$")
	chineseConformantRowPattern = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9.-]*)` \\|.*\\| 已通过一致性验证 \\|$")
)

func TestComponentMapsStaySynchronized(t *testing.T) {
	englishSlots := componentMapSlots(t, "COMPONENT_MAP.md")
	chineseSlots := componentMapSlots(t, "COMPONENT_MAP.zh-CN.md")
	englishContracted := componentMapRows(t, "COMPONENT_MAP.md", englishContractedRowPattern)
	chineseContracted := componentMapRows(t, "COMPONENT_MAP.zh-CN.md", chineseContractedRowPattern)

	assertSameSlotIDs(t, englishSlots, chineseSlots)
	assertSameSlotIDs(t, englishContracted, chineseContracted)
	englishConformant := componentMapRows(t, "COMPONENT_MAP.md", englishConformantRowPattern)
	chineseConformant := componentMapRows(t, "COMPONENT_MAP.zh-CN.md", chineseConformantRowPattern)
	assertSameSlotIDs(t, englishConformant, chineseConformant)
	if len(englishConformant) != 1 {
		t.Fatalf("Conformant row count = %d, want 1", len(englishConformant))
	}
	if _, exists := englishConformant["session.store"]; !exists {
		t.Fatal("session.store is missing its reviewed Conformant evidence status")
	}
}

func TestComponentMapsAreGeneratedFromCatalog(t *testing.T) {
	tests := []struct {
		path   string
		locale componentcatalog.Locale
	}{
		{path: "COMPONENT_MAP.md", locale: componentcatalog.LocaleEnglish},
		{path: "COMPONENT_MAP.zh-CN.md", locale: componentcatalog.LocaleChinese},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			current, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			generated, err := componentcatalog.RewriteMarkdown(tc.locale, current)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(current, generated) {
				t.Fatalf("%s drifted from ComponentCatalog; run go generate ./componentcatalog", tc.path)
			}
		})
	}
}

func TestPublicDocsDescribeAgentLoopAsAStandardProfileSlot(t *testing.T) {
	readme := readDocument(t, "README.md")

	if !strings.Contains(readme, "AgentLoop") {
		t.Fatalf("README does not describe standard profile slot %s", loop.AgentLoopSlot.ID())
	}
	if strings.Contains(readme, "standard `agent.loop` has been removed") {
		t.Fatalf("README says that standard slot %s was removed", loop.AgentLoopSlot.ID())
	}

	componentSlots := componentMapSlots(t, "COMPONENT_MAP.zh-CN.md")
	if _, exists := componentSlots[loop.AgentLoopSlot.ID()]; !exists {
		t.Fatalf("Chinese component map does not contain %s", loop.AgentLoopSlot.ID())
	}
}

func TestPublicDocsNameComponentCatalogAsTheStructuredSource(t *testing.T) {
	for _, path := range []string{"README.md", "COMPONENT_MAP.md", "COMPONENT_MAP.zh-CN.md"} {
		document := readDocument(t, path)
		if !strings.Contains(document, "ComponentCatalog") || !strings.Contains(document, "componentcatalog") {
			t.Fatalf("%s does not identify ComponentCatalog and its Go package", path)
		}
	}
}

func componentMapSlots(t *testing.T, path string) map[string]struct{} {
	t.Helper()

	return componentMapRows(t, path, componentMapRowPattern)
}

func componentMapRows(t *testing.T, path string, pattern *regexp.Regexp) map[string]struct{} {
	t.Helper()

	document := readDocument(t, path)
	slots := make(map[string]struct{})
	for _, match := range pattern.FindAllStringSubmatch(document, -1) {
		slots[match[1]] = struct{}{}
	}
	if len(slots) == 0 {
		t.Fatalf("%s contains no component-map rows", path)
	}
	return slots
}

func assertSameSlotIDs(t *testing.T, left, right map[string]struct{}) {
	t.Helper()

	for id := range left {
		if _, exists := right[id]; !exists {
			t.Errorf("Chinese component map is missing %s", id)
		}
	}
	for id := range right {
		if _, exists := left[id]; !exists {
			t.Errorf("English component map is missing %s", id)
		}
	}
}

func readDocument(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
