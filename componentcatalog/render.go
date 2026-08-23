package componentcatalog

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

type Locale string

const (
	LocaleEnglish Locale = "en"
	LocaleChinese Locale = "zh-CN"
)

var domainHeadings = map[Locale]map[string]string{
	LocaleEnglish: {
		"runtime": "### 1. Runtime and interaction", "model": "### 2. Model access",
		"tool": "### 3. Tools and skills", "context": "### 4. Context, history, and memory",
		"workspace": "### 5. Workspace, execution, and artifacts", "policy": "### 6. Policy, authorization, and human approval",
		"workflow": "### 7. Multi-agent and workflow", "billing": "### 9. Usage and billing",
		"operations": "### 10. Operations and audit",
	},
	LocaleChinese: {
		"runtime": "### 1. 运行时与交互", "model": "### 2. 模型访问",
		"tool": "### 3. 工具与技能", "context": "### 4. 上下文、历史与记忆",
		"workspace": "### 5. 工作区、执行与产物", "policy": "### 6. 策略、授权与人工审批",
		"workflow": "### 7. 多 Agent 与工作流", "billing": "### 9. 用量与计费",
		"operations": "### 10. 运维与审计",
	},
}

var orderedDomains = []string{"runtime", "model", "tool", "context", "workspace", "policy", "workflow", "billing", "operations"}

func RenderDomainTable(domain string, locale Locale) (string, error) {
	if !validLocale(locale) {
		return "", fmt.Errorf("componentcatalog: unsupported locale %q", locale)
	}
	components := componentsInDomain(domain)
	if len(components) == 0 {
		return "", fmt.Errorf("componentcatalog: unknown or empty domain %q", domain)
	}
	var output strings.Builder
	if locale == LocaleEnglish {
		output.WriteString("| Slot ID | Contract | Kind | Profile rule | Responsibility | Maturity |\n")
	} else {
		output.WriteString("| Slot ID | 契约 | 类型 | Profile 规则 | 职责 | 成熟度 |\n")
	}
	output.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, component := range components {
		text := component.Text.English
		maturity := string(component.Maturity)
		if locale == LocaleChinese {
			text = component.Text.Chinese
			maturity = chineseMaturity(component.Maturity)
		}
		fmt.Fprintf(&output, "| `%s` | `%s` | `%s` | %s | %s | %s |\n",
			component.ID, component.Contract.Symbol, component.Kind, text.ProfileRule, text.Responsibility, maturity)
	}
	return output.String(), nil
}

func RenderInventoryTable(locale Locale) (string, error) {
	if !validLocale(locale) {
		return "", fmt.Errorf("componentcatalog: unsupported locale %q", locale)
	}
	counts := Standard().Counts()
	var output strings.Builder
	if locale == LocaleEnglish {
		output.WriteString("| Inventory | Count |\n| --- | ---: |\n")
		fmt.Fprintf(&output, "| Mapped standard component ecosystems | %d |\n", len(Standard().Components))
		output.WriteString("| Standardized domain vocabularies | 9 |\n")
		fmt.Fprintf(&output, "| Contracted AgentSlot-owned domain interfaces | %d |\n", counts.AtLeastContracted())
		fmt.Fprintf(&output, "| Conformant component ecosystems | %d |\n| Proven component ecosystems | %d |\n| Assembled standard component ecosystems | %d |\n", counts.Conformant, counts.Proven, counts.Assembled)
	} else {
		output.WriteString("| 资产 | 数量 |\n| --- | ---: |\n")
		fmt.Fprintf(&output, "| 已映射的标准组件生态位 | %d |\n", len(Standard().Components))
		output.WriteString("| 已标准化的领域词汇 | 9 |\n")
		fmt.Fprintf(&output, "| 已定义契约的 AgentSlot 自有领域接口 | %d |\n", counts.AtLeastContracted())
		fmt.Fprintf(&output, "| 通过一致性验证的组件生态位 | %d |\n| 已由独立实现证明的组件生态位 | %d |\n| 已进入标准装配的组件生态位 | %d |\n", counts.Conformant, counts.Proven, counts.Assembled)
	}
	return output.String(), nil
}

func RenderStandardProfileTable(locale Locale) (string, error) {
	if !validLocale(locale) {
		return "", fmt.Errorf("componentcatalog: unsupported locale %q", locale)
	}
	order := []string{"agent.loop", "session.store", "model.executor", "model.token-counter", "gateway.channel"}
	var output strings.Builder
	if locale == LocaleEnglish {
		output.WriteString("| Slot ID | Standard contract | Kind | Required cardinality | Responsibility |\n")
	} else {
		output.WriteString("| Slot ID | 标准契约 | 类型 | 必需数量 | 职责 |\n")
	}
	output.WriteString("| --- | --- | --- | --- | --- |\n")
	catalog := Standard()
	for _, id := range order {
		component, ok := catalog.Lookup(id)
		if !ok || len(component.Profiles) == 0 {
			return "", fmt.Errorf("componentcatalog: standard profile component %q is missing", id)
		}
		text := component.Text.English
		cardinality := "exactly 1"
		if component.Kind != KindOne {
			cardinality = fmt.Sprintf("at least %d", component.Profiles[0].Minimum)
		}
		if locale == LocaleChinese {
			text = component.Text.Chinese
			cardinality = "恰好 1 个"
			if component.Kind != KindOne {
				cardinality = fmt.Sprintf("至少 %d 个", component.Profiles[0].Minimum)
			}
		}
		fmt.Fprintf(&output, "| `%s` | `%s` | `%s` | %s | %s |\n", component.ID, component.Contract.Symbol, component.Kind, cardinality, text.Responsibility)
	}
	return output.String(), nil
}

func RewriteMarkdown(locale Locale, source []byte) ([]byte, error) {
	if !validLocale(locale) {
		return nil, fmt.Errorf("componentcatalog: unsupported locale %q", locale)
	}
	result := string(source)
	inventory, _ := RenderInventoryTable(locale)
	profile, _ := RenderStandardProfileTable(locale)
	if locale == LocaleEnglish {
		result = replaceTableAfter(result, "Current repository reality:", "| Inventory |", inventory)
		result = replaceTableAfter(result, "## Runnable standard profile", "| Slot ID | Standard contract |", profile)
	} else {
		result = replaceTableAfter(result, "仓库当前真实状态", "| 资产 |", inventory)
		result = replaceTableAfter(result, "## 可运行标准 Profile", "| Slot ID | 标准契约 |", profile)
	}
	for _, domain := range orderedDomains {
		table, err := RenderDomainTable(domain, locale)
		if err != nil {
			return nil, err
		}
		heading := domainHeadings[locale][domain]
		header := "| Slot ID | Contract |"
		if locale == LocaleChinese {
			header = "| Slot ID | 契约 |"
		}
		result = replaceTableAfter(result, heading, header, table)
	}
	return bytes.Clone([]byte(result)), nil
}

func componentsInDomain(domain string) []Component {
	var result []Component
	for _, component := range Standard().Components {
		if component.Domain == domain {
			result = append(result, component)
		}
	}
	return result
}

func validLocale(locale Locale) bool { return locale == LocaleEnglish || locale == LocaleChinese }

func chineseMaturity(maturity Maturity) string {
	switch maturity {
	case MaturityMapped:
		return "已映射"
	case MaturityContracted:
		return "已定义契约"
	case MaturityConformant:
		return "已通过一致性验证"
	case MaturityProven:
		return "已跨实现证明"
	case MaturityAssembled:
		return "已真实装配"
	default:
		return string(maturity)
	}
}

func replaceTableAfter(document, anchor, headerPrefix, replacement string) string {
	anchorIndex := strings.Index(document, anchor)
	if anchorIndex < 0 {
		return document
	}
	headerRelative := strings.Index(document[anchorIndex:], headerPrefix)
	if headerRelative < 0 {
		return document
	}
	start := anchorIndex + headerRelative
	end := start
	for end < len(document) {
		lineEnd := strings.IndexByte(document[end:], '\n')
		if lineEnd < 0 {
			lineEnd = len(document) - end
		}
		line := document[end : end+lineEnd]
		if !strings.HasPrefix(line, "|") {
			break
		}
		end += lineEnd
		if end < len(document) && document[end] == '\n' {
			end++
		}
	}
	return document[:start] + replacement + document[end:]
}

func Domains() []string {
	result := append([]string(nil), orderedDomains...)
	sort.Strings(result)
	return result
}
