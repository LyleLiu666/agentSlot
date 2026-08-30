package componentcatalog

import (
	"strings"
	"testing"
)

func TestRenderDomainTableUsesCatalogFacts(t *testing.T) {
	table, err := RenderDomainTable("model", LocaleEnglish)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(table, "| Slot ID | Contract | Kind | Profile rule | Responsibility | Maturity |\n") {
		t.Fatalf("unexpected table header:\n%s", table)
	}
	if got := strings.Count(table, "\n| `"); got != 7 {
		t.Fatalf("model row count = %d, want 7\n%s", got, table)
	}
	for _, fragment := range []string{"`model.token-counter`", "`TokenCounter`", "Contracted", "failing closed"} {
		if !strings.Contains(table, fragment) {
			t.Fatalf("model table lacks %q:\n%s", fragment, table)
		}
	}
}

func TestRewriteMarkdownReplacesInventoryProfileAndDomainTables(t *testing.T) {
	source := `# Map

Current repository reality:

| Inventory | Count |
| --- | ---: |
| stale | 999 |

## Runnable standard profile

| Slot ID | Standard contract | Kind | Required cardinality | Responsibility |
| --- | --- | --- | --- | --- |
| stale | stale | stale | stale | stale |

## Component ecosystems

### 2. Model access

| Slot ID | Contract | Kind | Profile rule | Responsibility | Maturity |
| --- | --- | --- | --- | --- | --- |
| stale | stale | stale | stale | stale | stale |

After.
`

	result, err := RewriteMarkdown(LocaleEnglish, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	text := string(result)
	if strings.Contains(text, "stale") || !strings.Contains(text, "Mapped standard component ecosystems | 42") || !strings.Contains(text, "`model.token-counter`") || !strings.Contains(text, "After.") {
		t.Fatalf("RewriteMarkdown result:\n%s", text)
	}
	second, err := RewriteMarkdown(LocaleEnglish, result)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != text {
		t.Fatal("RewriteMarkdown is not deterministic")
	}
}

func TestRewriteChineseInventoryUsesPublishedHeadings(t *testing.T) {
	source := `仓库当前真实状态：

| 资产 | 数量 |
| --- | ---: |
| stale | 999 |
`
	result, err := RewriteMarkdown(LocaleChinese, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "stale") || !strings.Contains(string(result), "| 已映射的标准组件生态位 | 42 |") {
		t.Fatalf("Chinese inventory was not generated:\n%s", result)
	}
}

func TestRenderRejectsUnknownLocaleAndDomain(t *testing.T) {
	if _, err := RenderDomainTable("unknown", LocaleEnglish); err == nil {
		t.Fatal("unknown domain was accepted")
	}
	if _, err := RewriteMarkdown("unknown", []byte("map")); err == nil {
		t.Fatal("unknown locale was accepted")
	}
}
