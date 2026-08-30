// Package componentcatalog exposes AgentSlot's versioned standard component
// inventory. It describes portable extension boundaries; it never contains
// component instances or participates in application assembly.
package componentcatalog

import (
	"fmt"
	"regexp"
	"slices"
)

const StandardVersion = "agentslot.component-catalog/v1"

type Kind string

const (
	KindOne   Kind = "One"
	KindMany  Kind = "Many"
	KindChain Kind = "Chain"
)

func (k Kind) valid() bool { return k == KindOne || k == KindMany || k == KindChain }

type Maturity string

const (
	MaturityMapped     Maturity = "Mapped"
	MaturityContracted Maturity = "Contracted"
	MaturityConformant Maturity = "Conformant"
	MaturityProven     Maturity = "Proven"
	MaturityAssembled  Maturity = "Assembled"
)

func (m Maturity) valid() bool {
	return m == MaturityMapped || m == MaturityContracted || m == MaturityConformant || m == MaturityProven || m == MaturityAssembled
}

type LocalizedText struct {
	ProfileRule    string
	Responsibility string
}

type Text struct {
	English LocalizedText
	Chinese LocalizedText
}

type ContractRef struct {
	Package   string
	Symbol    string
	Available bool
}

type ProfileRequirement struct {
	Name    string
	Minimum int
}

type Evidence struct {
	ConformanceSuite           string
	ConformanceEvidence        []string
	IndependentImplementations []string
	AssemblyEvidence           []string
	KnownGaps                  []string
}

type Component struct {
	Domain   string
	ID       string
	Kind     Kind
	Contract ContractRef
	Maturity Maturity
	Profiles []ProfileRequirement
	Text     Text
	Evidence Evidence
}

// ConfigurationField describes one non-secret value required to emit an
// implementation. Credential material is deliberately not representable.
type ConfigurationField struct {
	Key         string
	Description string
	Required    bool
}

// Implementation is scaffold metadata for one publicly constructible
// implementation. It contains no component instance or configuration value.
type Implementation struct {
	ID                string
	ComponentID       string
	Package           string
	Symbol            string
	Available         bool
	UnavailableReason string
	Dependencies      []string
	Conflicts         []string
	Configuration     []ConfigurationField
	ToolKeys          []string
}

// Preset is one deterministic, reviewable scaffold selection. ToolKeys is the
// explicit allowlist emitted by the generator; it is never a Runtime default.
type Preset struct {
	ID              string
	Title           string
	Implementations []string
	ToolKeys        []string
}

type Catalog struct {
	StandardVersion string
	Components      []Component
	Implementations []Implementation
	Presets         []Preset
}

type MaturityCounts struct {
	Mapped     int
	Contracted int
	Conformant int
	Proven     int
	Assembled  int
}

func (c MaturityCounts) AtLeastContracted() int {
	return c.Contracted + c.Conformant + c.Proven + c.Assembled
}

func (c Catalog) Counts() MaturityCounts {
	var result MaturityCounts
	for _, component := range c.Components {
		switch component.Maturity {
		case MaturityMapped:
			result.Mapped++
		case MaturityContracted:
			result.Contracted++
		case MaturityConformant:
			result.Conformant++
		case MaturityProven:
			result.Proven++
		case MaturityAssembled:
			result.Assembled++
		}
	}
	return result
}

func (c Catalog) Lookup(id string) (Component, bool) {
	for _, component := range c.Components {
		if component.ID == id {
			return cloneComponent(component), true
		}
	}
	return Component{}, false
}

func (c Catalog) LookupImplementation(id string) (Implementation, bool) {
	for _, implementation := range c.Implementations {
		if implementation.ID == id {
			return cloneImplementation(implementation), true
		}
	}
	return Implementation{}, false
}

func (c Catalog) LookupPreset(id string) (Preset, bool) {
	for _, preset := range c.Presets {
		if preset.ID == id {
			return clonePreset(preset), true
		}
	}
	return Preset{}, false
}

var componentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)

func (c Catalog) Validate() error {
	if c.StandardVersion != StandardVersion {
		return fmt.Errorf("componentcatalog: unsupported standard version %q", c.StandardVersion)
	}
	if len(c.Components) == 0 {
		return fmt.Errorf("componentcatalog: at least one component is required")
	}
	seen := make(map[string]struct{}, len(c.Components))
	for index, component := range c.Components {
		if !componentIDPattern.MatchString(component.ID) {
			return fmt.Errorf("componentcatalog: component %d has invalid ID %q", index, component.ID)
		}
		if _, duplicate := seen[component.ID]; duplicate {
			return fmt.Errorf("componentcatalog: duplicate component ID %q", component.ID)
		}
		seen[component.ID] = struct{}{}
		if component.Domain == "" || !component.Kind.valid() || !component.Maturity.valid() {
			return fmt.Errorf("componentcatalog: component %q has invalid domain, kind, or maturity", component.ID)
		}
		if component.Contract.Symbol == "" {
			return fmt.Errorf("componentcatalog: component %q has no contract symbol", component.ID)
		}
		if component.Maturity != MaturityMapped && (!component.Contract.Available || component.Contract.Package == "") {
			return fmt.Errorf("componentcatalog: component %q claims maturity without an available public contract", component.ID)
		}
		if component.Text.English.ProfileRule == "" || component.Text.English.Responsibility == "" ||
			component.Text.Chinese.ProfileRule == "" || component.Text.Chinese.Responsibility == "" {
			return fmt.Errorf("componentcatalog: component %q lacks localized public text", component.ID)
		}
		profiles := make(map[string]struct{}, len(component.Profiles))
		for _, profile := range component.Profiles {
			if profile.Name == "" || profile.Minimum < 1 || (component.Kind == KindOne && profile.Minimum != 1) {
				return fmt.Errorf("componentcatalog: component %q has invalid profile requirement", component.ID)
			}
			if _, duplicate := profiles[profile.Name]; duplicate {
				return fmt.Errorf("componentcatalog: component %q repeats profile %q", component.ID, profile.Name)
			}
			profiles[profile.Name] = struct{}{}
		}
		if component.Maturity == MaturityConformant || component.Maturity == MaturityProven || component.Maturity == MaturityAssembled {
			if component.Evidence.ConformanceSuite == "" || len(component.Evidence.ConformanceEvidence) == 0 {
				return fmt.Errorf("componentcatalog: component %q lacks conformance evidence", component.ID)
			}
		}
		if (component.Maturity == MaturityProven || component.Maturity == MaturityAssembled) && len(component.Evidence.IndependentImplementations) < 2 {
			return fmt.Errorf("componentcatalog: component %q lacks two independent implementations", component.ID)
		}
		if component.Maturity == MaturityAssembled && len(component.Evidence.AssemblyEvidence) == 0 {
			return fmt.Errorf("componentcatalog: component %q lacks assembly evidence", component.ID)
		}
	}
	implementations, err := c.validateImplementations()
	if err != nil {
		return err
	}
	return c.validatePresets(implementations)
}

func (c Catalog) validateImplementations() (map[string]Implementation, error) {
	implementations := make(map[string]Implementation, len(c.Implementations))
	for _, implementation := range c.Implementations {
		if !componentIDPattern.MatchString(implementation.ID) || implementation.Package == "" || implementation.Symbol == "" {
			return nil, fmt.Errorf("componentcatalog: invalid implementation %q", implementation.ID)
		}
		if _, duplicate := implementations[implementation.ID]; duplicate {
			return nil, fmt.Errorf("componentcatalog: duplicate implementation %q", implementation.ID)
		}
		component, ok := c.Lookup(implementation.ComponentID)
		if !ok || (implementation.Available && !component.Contract.Available) {
			return nil, fmt.Errorf("componentcatalog: implementation %q has an unavailable component", implementation.ID)
		}
		if implementation.Available == (implementation.UnavailableReason != "") {
			return nil, fmt.Errorf("componentcatalog: implementation %q has inconsistent availability metadata", implementation.ID)
		}
		for _, dependency := range implementation.Dependencies {
			if _, ok := c.Lookup(dependency); !ok {
				return nil, fmt.Errorf("componentcatalog: implementation %q has unknown dependency %q", implementation.ID, dependency)
			}
		}
		fields := make(map[string]struct{}, len(implementation.Configuration))
		for _, field := range implementation.Configuration {
			if !componentIDPattern.MatchString(field.Key) || field.Description == "" {
				return nil, fmt.Errorf("componentcatalog: implementation %q has invalid configuration field %q", implementation.ID, field.Key)
			}
			if _, duplicate := fields[field.Key]; duplicate {
				return nil, fmt.Errorf("componentcatalog: implementation %q repeats configuration field %q", implementation.ID, field.Key)
			}
			fields[field.Key] = struct{}{}
		}
		tools := make(map[string]struct{}, len(implementation.ToolKeys))
		for _, key := range implementation.ToolKeys {
			if key == "" {
				return nil, fmt.Errorf("componentcatalog: implementation %q has an empty Tool key", implementation.ID)
			}
			if _, duplicate := tools[key]; duplicate {
				return nil, fmt.Errorf("componentcatalog: implementation %q repeats Tool key %q", implementation.ID, key)
			}
			tools[key] = struct{}{}
		}
		implementations[implementation.ID] = implementation
	}
	for _, implementation := range implementations {
		for _, conflict := range implementation.Conflicts {
			if _, ok := implementations[conflict]; !ok || conflict == implementation.ID {
				return nil, fmt.Errorf("componentcatalog: implementation %q has invalid conflict %q", implementation.ID, conflict)
			}
		}
	}
	return implementations, nil
}

func (c Catalog) validatePresets(implementations map[string]Implementation) error {
	presets := make(map[string]struct{}, len(c.Presets))
	for _, preset := range c.Presets {
		if !componentIDPattern.MatchString(preset.ID) || preset.Title == "" || len(preset.Implementations) == 0 {
			return fmt.Errorf("componentcatalog: invalid preset %q", preset.ID)
		}
		if _, duplicate := presets[preset.ID]; duplicate {
			return fmt.Errorf("componentcatalog: duplicate preset %q", preset.ID)
		}
		selectedComponents := make(map[string]int)
		selectedImplementations := make(map[string]struct{})
		for _, id := range preset.Implementations {
			implementation, ok := implementations[id]
			if !ok || !implementation.Available {
				return fmt.Errorf("componentcatalog: preset %q selects unavailable implementation %q", preset.ID, id)
			}
			if _, duplicate := selectedImplementations[id]; duplicate {
				return fmt.Errorf("componentcatalog: preset %q repeats implementation %q", preset.ID, id)
			}
			selectedImplementations[id] = struct{}{}
			selectedComponents[implementation.ComponentID]++
		}
		for _, id := range preset.Implementations {
			for _, dependency := range implementations[id].Dependencies {
				if selectedComponents[dependency] == 0 {
					return fmt.Errorf("componentcatalog: preset %q omits dependency %q", preset.ID, dependency)
				}
			}
			for _, conflict := range implementations[id].Conflicts {
				if _, selected := selectedImplementations[conflict]; selected {
					return fmt.Errorf("componentcatalog: preset %q selects conflicting implementations %q and %q", preset.ID, id, conflict)
				}
			}
		}
		for _, component := range c.Components {
			for _, requirement := range component.Profiles {
				if requirement.Name == "standard-agent" && selectedComponents[component.ID] < requirement.Minimum {
					return fmt.Errorf("componentcatalog: preset %q omits standard requirement %q", preset.ID, component.ID)
				}
			}
			if component.Kind == KindOne && selectedComponents[component.ID] > 1 {
				return fmt.Errorf("componentcatalog: preset %q selects multiple implementations for One component %q", preset.ID, component.ID)
			}
		}
		seenTools := make(map[string]struct{}, len(preset.ToolKeys))
		availableTools := make(map[string]struct{})
		for id := range selectedImplementations {
			for _, key := range implementations[id].ToolKeys {
				availableTools[key] = struct{}{}
			}
		}
		for _, key := range preset.ToolKeys {
			if key == "" {
				return fmt.Errorf("componentcatalog: preset %q has an empty Tool key", preset.ID)
			}
			if _, duplicate := seenTools[key]; duplicate {
				return fmt.Errorf("componentcatalog: preset %q repeats Tool key %q", preset.ID, key)
			}
			if _, available := availableTools[key]; !available {
				return fmt.Errorf("componentcatalog: preset %q exposes unknown Tool key %q", preset.ID, key)
			}
			seenTools[key] = struct{}{}
		}
		presets[preset.ID] = struct{}{}
	}
	return nil
}

func Standard() Catalog {
	result := Catalog{
		StandardVersion: standardCatalog.StandardVersion,
		Components:      make([]Component, len(standardCatalog.Components)),
		Implementations: make([]Implementation, len(standardCatalog.Implementations)),
		Presets:         make([]Preset, len(standardCatalog.Presets)),
	}
	for index, component := range standardCatalog.Components {
		result.Components[index] = cloneComponent(component)
	}
	for index, implementation := range standardCatalog.Implementations {
		result.Implementations[index] = cloneImplementation(implementation)
	}
	for index, preset := range standardCatalog.Presets {
		result.Presets[index] = clonePreset(preset)
	}
	return result
}

func cloneComponent(component Component) Component {
	component.Profiles = slices.Clone(component.Profiles)
	component.Evidence.ConformanceEvidence = slices.Clone(component.Evidence.ConformanceEvidence)
	component.Evidence.IndependentImplementations = slices.Clone(component.Evidence.IndependentImplementations)
	component.Evidence.AssemblyEvidence = slices.Clone(component.Evidence.AssemblyEvidence)
	component.Evidence.KnownGaps = slices.Clone(component.Evidence.KnownGaps)
	return component
}

func cloneImplementation(implementation Implementation) Implementation {
	implementation.Dependencies = slices.Clone(implementation.Dependencies)
	implementation.Conflicts = slices.Clone(implementation.Conflicts)
	implementation.Configuration = slices.Clone(implementation.Configuration)
	implementation.ToolKeys = slices.Clone(implementation.ToolKeys)
	return implementation
}

func clonePreset(preset Preset) Preset {
	preset.Implementations = slices.Clone(preset.Implementations)
	preset.ToolKeys = slices.Clone(preset.ToolKeys)
	return preset
}

func profile(minimum int) []ProfileRequirement {
	return []ProfileRequirement{{Name: "standard-agent", Minimum: minimum}}
}

func entry(domain, id, symbol string, kind Kind, maturity Maturity, packagePath string, available bool, profiles []ProfileRequirement, enRule, enResponsibility, zhRule, zhResponsibility string) Component {
	knownGaps := []string{"no conformance evidence", "no two independent implementations", "no approved real-consumer replacement evidence"}
	if maturity == MaturityMapped {
		knownGaps = append([]string{"public contract not yet available"}, knownGaps...)
	}
	return Component{
		Domain: domain, ID: id, Kind: kind,
		Contract: ContractRef{Package: packagePath, Symbol: symbol, Available: available},
		Maturity: maturity, Profiles: profiles,
		Text: Text{
			English: LocalizedText{ProfileRule: enRule, Responsibility: enResponsibility},
			Chinese: LocalizedText{ProfileRule: zhRule, Responsibility: zhResponsibility},
		},
		Evidence: Evidence{KnownGaps: knownGaps},
	}
}

func withEvidence(component Component, evidence Evidence) Component {
	component.Evidence = evidence
	return component
}

const module = "github.com/LyleLiu666/agentSlot"

var standardCatalog = Catalog{StandardVersion: StandardVersion, Components: []Component{
	entry("runtime", "agent.loop", "AgentLoop", KindOne, MaturityContracted, module+"/loop", true, profile(1), "globally requires exactly 1", "Owns replaceable Agent execution strategy through ordered, Run-scoped Runtime actions while the framework retains Session truth, budgets, cancellation, recovery, and terminal commits.", "全局恰好 1 个", "通过有序、Run-scoped 的受限 Runtime actions 承载可替换执行策略；框架继续独占 Session 真相、预算、取消、恢复和终态提交。"),
	entry("runtime", "gateway.channel", "GatewayChannel", KindMany, MaturityContracted, module+"/interaction", true, profile(1), "globally requires at least 1", "Binds one caller-facing protocol, function API, or UI to the fixed Gateway and receives only `GatewayAccess`; gRPC, WebSocket, SSH, and inbound ACP are alternative implementations of this Slot.", "全局至少 1 个", "把调用方协议、函数 API 或 UI 绑定到固定 Gateway，并且只能取得 `GatewayAccess`；gRPC、WebSocket、SSH 和入站 ACP 都是该 Slot 的不同实现。"),
	entry("runtime", "interaction.command", "InteractionCommand", KindMany, MaturityContracted, module+"/interaction", true, nil, "optional", "Registers a keyed UI-neutral command with the fixed Gateway; Channels render the shared descriptor as slash commands, menus, buttons, forms, or command palettes.", "可选", "向固定 Gateway 注册具名、UI-neutral 的结构化命令；Channel 把共享描述渲染为 Slash、菜单、按钮、表单或命令面板。"),
	entry("runtime", "agent.hook", "AgentHook", KindChain, MaturityContracted, module+"/hook", true, nil, "optional", "Proposes controlled follow-on input before run completion; it cannot mutate Session state or become a second Runtime controller.", "可选", "在 Run 完成前提出受控的后续输入；不能修改 Session 状态，也不能成为第二个 Runtime 控制者。"),
	entry("runtime", "hook.input_gate", "InputGate", KindChain, MaturityContracted, module+"/hook", true, nil, "optional", "Accepts or rejects a proposed Send, Steer, or queued edit before mutation and may contribute bounded context to the exact Run/Step that later claims the unchanged input.", "可选", "在 Send、Steer 或排队输入编辑生效前接受或拒绝，并可为之后认领未改写输入的精确 Run/Step 提供有界上下文。"),
	entry("runtime", "hook.tool_preflight", "ToolPreflight", KindChain, MaturityContracted, module+"/hook", true, nil, "optional", "Provides durable ordered allow, deny, or approval advice after ToolCall preparation and before policy authorization or execution; it cannot replace the call or bypass policy.", "可选", "在 ToolCall prepared 之后、Policy 授权或执行之前提供持久、有序的允许、拒绝或审批建议；不能替换调用或绕过 Policy。"),
	entry("runtime", "hook.tool_result", "ToolResultHook", KindChain, MaturityContracted, module+"/hook", true, nil, "optional", "Observes only certain succeeded or failed results from tools that actually entered execution and may contribute bounded context to the exact next Step without rewriting ToolResult.", "可选", "只处理确实进入执行并形成 succeeded/failed 的确定 ToolResult，可向精确下一 Step 提供有界上下文，但不能改写 ToolResult。"),
	entry("runtime", "goal.store", "goal.Store", KindOne, MaturityContracted, module+"/goal", true, nil, "optional; installed with `goal.evaluator`", "Owns one CAS-protected objective lifecycle per Session, separate from append-only conversation History.", "可选；与 `goal.evaluator` 同时安装", "为每个 Session 保存一份受 CAS 保护的目标生命周期，与仅追加的会话 History 分离。"),
	entry("runtime", "goal.evaluator", "goal.Evaluator", KindOne, MaturityContracted, module+"/goal", true, nil, "optional; installed with `goal.store`", "Makes a structured continue/blocked/done decision before an otherwise finished Run closes.", "可选；与 `goal.store` 同时安装", "在本来准备结束的 Run 关闭前，给出结构化的继续、阻塞或完成判断。"),
	entry("runtime", "session.commit.observer", "SessionCommitObserver", KindChain, MaturityContracted, module+"/session", true, nil, "optional", "Asynchronously observes applied Session revisions and their appended History sequence ranges; failures and panics cannot roll back a commit.", "可选", "异步观察已经生效的 Session revision 及其新增 History sequence 范围；错误和 panic 不能回滚提交。"),

	withEvidence(entry("model", "model.executor", "ModelExecutor", KindOne, MaturityConformant, module+"/model", true, profile(1), "globally required", "Validates selected-model capabilities, executes one logical model call with a checked temporary/reset/terminal stream, contains retries and continuation, reports post-call usage, and durably records each physical attempt plus an optional bounded adapter-sanitized displayable failure message through the restricted AttemptRecorder.", "全局必需", "校验所选模型能力，以受校验的临时输出/reset/终态流执行一次逻辑模型调用，封装重试和续传、报告调用后 Usage，并通过受限 AttemptRecorder 持久记录每次真实请求及可选、受限且由适配器清洗后允许展示的失败消息。"), Evidence{
		ConformanceSuite:    "model.executor/v1",
		ConformanceEvidence: []string{"agentSlot@v0.1.8"},
		KnownGaps:           []string{"no second semantically independent production implementation", "no approved real-consumer replacement evidence"},
	}),
	entry("model", "model.token-counter", "TokenCounter", KindOne, MaturityContracted, module+"/model", true, profile(1), "globally requires exactly 1", "Counts the complete provider-visible request for pre-call planning, using an exact tokenizer or a validated conservative bound and failing closed when neither is defensible.", "全局恰好 1 个", "为调用前规划计量完整 Provider 可见请求；使用精确 tokenizer 或经过验证的保守上界，两者都不可信时 fail closed。"),
	entry("model", "model.attempt.observer", "AttemptObserver", KindChain, MaturityContracted, module+"/model", true, nil, "optional", "Synchronously records or rejects one physical provider attempt before dispatch and after completion; unlike passive telemetry it may fail closed.", "可选", "在每次真实 Provider 请求发送前和结束后同步记录或拒绝；与被动遥测不同，它可以 fail closed。"),
	entry("model", "model.provider", "ModelProvider", KindMany, MaturityMapped, "", false, nil, "optional; required only by an Executor that declares it", "Implements named provider access for Executors that compose local adapters.", "可选；仅由声明依赖的 Executor 要求", "为组合本地适配器的 Executor 提供具名 Provider 访问。"),
	entry("model", "model.selector", "ModelSelector", KindOne, MaturityMapped, "", false, nil, "optional; conditional for dynamic routing", "Selects a provider/model using explicit request and policy inputs.", "可选；动态路由时按条件要求", "根据明确的请求和策略输入选择 Provider/模型。"),
	entry("model", "model.catalog", "ModelCatalog", KindMany, MaturityContracted, module+"/model", true, nil, "optional", "Describes available models and their declared capabilities without exposing credentials.", "可选", "描述可用模型及其声明能力，但不暴露凭证。"),
	entry("model", "model.middleware", "ModelMiddleware", KindChain, MaturityMapped, "", false, nil, "optional", "Applies observable request/response concerns without changing provider identity.", "可选", "在不改变 Provider 身份的前提下处理可观察的请求/响应横切逻辑。"),

	entry("tool", "tool", "Tool", KindMany, MaturityContracted, module+"/tool", true, nil, "optional globally; profiles may require keys", "Declares and invokes a named capability available to AgentRuntime.", "全局可选；Profile 可要求指定键", "声明并调用一个可供 AgentRuntime 使用的具名能力。"),
	entry("tool", "skill", "Skill", KindMany, MaturityMapped, "", false, nil, "optional", "Supplies discoverable instructions, resources, or component bundles without pretending natural-language keyword matching is semantic routing.", "可选", "提供可发现的指令、资源或组件包，不能用自然语言关键字匹配冒充语义路由。"),
	entry("tool", "tool.middleware", "ToolMiddleware", KindChain, MaturityMapped, "", false, nil, "optional", "Wraps invocation for policy, telemetry, normalization, or recovery.", "可选", "为调用过程增加策略、遥测、标准化或恢复处理。"),

	withEvidence(entry("context", "session.store", "SessionStore", KindOne, MaturityContracted, module+"/session", true, profile(1), "globally required", "Persists the whole Session aggregate and atomic revision/CAS transactions, including typed extension recovery records and bounded sequence diagnostics; preserves tool arguments by JSON value semantics across encoding and reopen; History remains the unique append-only fact view.", "全局必需", "持久化完整 Session 聚合及其 revision/CAS 原子事务，包括类型化扩展恢复记录和有界 sequence 诊断；跨编码与重开按 JSON 值语义保持工具参数；History 仍是唯一、append-only 的事实视图。"), Evidence{
		ConformanceSuite:    "session.store/v1",
		ConformanceEvidence: []string{"agentSlot@c6b42a767d5422464ebc2978bf408b7d15eb5125"},
		KnownGaps:           []string{"evidence covers the pre-extension v1 contract", "complete session.store/v2 suite not yet published", "no second semantically independent implementation", "no approved real-consumer replacement evidence"},
	}),
	entry("context", "context.source", "ContextSource", KindChain, MaturityContracted, module+"/context", true, nil, "optional", "Contributes ordered context for a model turn.", "可选", "为一次模型调用按顺序提供上下文。"),
	entry("context", "context.compactor", "ContextCompactor", KindOne, MaturityContracted, module+"/context", true, nil, "optional", "Replaces the current full Context with a smaller conversation-message projection without rewriting History; AgentRuntime reattaches fixed prompts/tools and validates protocol and hard token limits.", "可选", "把当前完整 Context 转为更小的会话消息投影且不改写 History；AgentRuntime 重新装配固定 Prompt/Tool，并校验协议和硬 Token 上限。"),
	entry("context", "memory.store", "MemoryStore", KindMany, MaturityContracted, module+"/memory", true, nil, "optional", "Recalls, remembers, and forgets governed long-term memory outside authoritative conversation History.", "可选", "在权威会话 History 之外召回、记住和遗忘受治理的长期记忆。"),
	entry("context", "checkpoint.store", "CheckpointStore", KindOne, MaturityMapped, "", false, nil, "optional", "Saves resumable execution state without pretending it is user-visible history.", "可选", "保存可恢复的执行状态，但不把它冒充为用户可见的历史。"),

	entry("workspace", "workspace.manager", "workspace.Manager", KindOne, MaturityContracted, module+"/workspace", true, nil, "optional", "Resolves and isolates the trusted resource boundary visible to a Session or Run; a Workspace may be a local directory, container, remote resource, cloud notes, or object storage, while concrete operations remain separate components.", "可选", "解析并隔离 Session 或 Run 可见的可信资源边界；Workspace 可以是本地目录、容器、远程资源、云笔记或对象存储，具体操作继续属于独立组件。"),
	entry("workspace", "execution.environment", "ExecutionEnvironment", KindMany, MaturityMapped, "", false, nil, "optional", "Executes commands or code in a named local, container, sandbox, or remote environment.", "可选", "在具名的本地、容器、沙箱或远程环境中执行命令或代码。"),
	entry("workspace", "artifact.store", "ArtifactStore", KindOne, MaturityContracted, module+"/artifact", true, nil, "optional; required by components that consume attachments", "Persists immutable inbound or generated content—including tool content deliberately retained long-term—and resolves stable metadata/references without placing binary data, local paths, or credentials in History.", "可选；消费附件的组件必须依赖", "持久化不可变的输入附件、生成内容及明确长期保留的工具内容，通过稳定元数据和引用读取；History 不保存二进制、本地路径或凭据。"),
	entry("workspace", "credential.resolver", "Resolver", KindOne, MaturityContracted, module+"/credential", true, nil, "optional; required by configured outbound adapters", "Late-resolves a product-supplied non-secret Ref inside one physical outbound-operation callback; supports distinct credential shapes while exposing only an opaque non-reversible identity outside that callback.", "可选；配置了凭据引用的外部适配器必须依赖", "在一次真实外部操作的回调内晚绑定产品提供的非秘密 Ref；支持不同凭据形态，回调外只暴露不透明且不可逆的安全 identity。"),

	entry("policy", "policy.guard", "PolicyGuard", KindChain, MaturityContracted, module+"/policy", true, nil, "optional", "Evaluates a detached proposed tool action in deterministic order without gaining execution authority.", "可选", "按确定顺序评估隔离副本形式的拟议工具动作，但不取得工具执行权。"),
	entry("policy", "approval.service", "ApprovalService", KindOne, MaturityContracted, module+"/policy", true, nil, "optional; risk profiles may require it", "Resolves an approval request after policy requires confirmation, independently of a particular UI.", "可选；高风险 Profile 可要求", "在策略要求确认后解析审批请求，不依赖某个具体 UI。"),
	entry("policy", "authorization.provider", "AuthorizationProvider", KindOne, MaturityMapped, "", false, nil, "optional", "Decides whether an authenticated principal may perform an agent operation.", "可选", "判断已认证主体是否有权执行某项 Agent 操作。"),

	entry("workflow", "agent.provider", "AgentProvider", KindMany, MaturityContracted, module+"/workflow", true, nil, "optional", "Executes a task through a named child-agent or remote-agent implementation.", "可选", "通过具名的子 Agent 或远程 Agent 实现执行任务。"),
	entry("workflow", "workflow.scheduler", "Scheduler", KindOne, MaturityContracted, module+"/workflow", true, nil, "optional", "Schedules asynchronous multi-agent work without replacing the fixed per-Session AgentRuntime.", "可选", "异步调度多 Agent 工作，但不替代框架固定的每 Session AgentRuntime。"),
	entry("workflow", "job.store", "JobStore", KindOne, MaturityContracted, module+"/workflow", true, nil, "optional", "Persists CAS-versioned queued/running/terminal workflow job state and wait notifications.", "可选", "持久化带 CAS version 的排队、运行和终态 Job，并提供等待通知。"),
	entry("workflow", "mailbox", "Mailbox", KindOne, MaturityContracted, module+"/workflow", true, nil, "optional", "Carries append-only, addressed asynchronous messages between Sessions and jobs.", "可选", "在 Session 与 Job 之间传递仅追加、有明确收件人的异步消息。"),

	entry("billing", "usage.recorder", "UsageRecorder", KindChain, MaturityContracted, module+"/observe", true, nil, "optional", "Receives Provider-reported token usage for one identified physical model attempt.", "可选", "接收由 Provider 报告、属于某次具名物理模型 Attempt 的 Token 用量。"),
	entry("billing", "price.resolver", "PriceResolver", KindOne, MaturityContracted, module+"/billing", true, nil, "optional", "Resolves an integer-micro, currency- and version-labelled price for normalized usage.", "可选", "为标准化用量解析带币种、版本和整数微单位的价格。"),
	entry("billing", "quota.guard", "QuotaGuard", KindOne, MaturityContracted, module+"/billing", true, nil, "optional", "Checks, reserves, commits, or releases explicitly attributed quota before provider work.", "可选", "在 Provider 工作发生前，对明确归属的额度执行检查、预留、提交或释放。"),
	entry("billing", "billing.ledger", "BillingLedger", KindOne, MaturityContracted, module+"/billing", true, nil, "optional", "Persists immutable physical-attempt intent and outcome facts for audit and later settlement.", "可选", "持久化每次真实模型 Attempt 的不可变 intent 与 outcome，供审计和后续结算。"),

	entry("operations", "audit.sink", "AuditSink", KindChain, MaturityContracted, module+"/observe", true, nil, "optional", "Receives model-config and tool-policy decision facts without message content or tool arguments.", "可选", "接收模型配置变更和工具策略决策事实，不包含消息内容或工具参数。"),
	entry("operations", "trace.sink", "TraceSink", KindChain, MaturityContracted, module+"/observe", true, nil, "optional", "Receives correlated Runtime, Run, model-attempt, and tool lifecycle facts.", "可选", "接收相互关联的 Runtime、Run、模型 Attempt 和工具生命周期事实。"),
	entry("operations", "metric.sink", "MetricSink", KindChain, MaturityContracted, module+"/observe", true, nil, "optional", "Receives normalized counters and duration measurements with detached attributes.", "可选", "接收带隔离属性副本的标准化计数与耗时度量。"),
	entry("operations", "health.contributor", "HealthContributor", KindChain, MaturityMapped, "", false, nil, "optional", "Reports component readiness and health without exposing configuration values.", "可选", "报告组件就绪状态与健康状况，但不暴露配置值。"),
}, Implementations: []Implementation{
	{ID: "standard.loop", ComponentID: "agent.loop", Package: module + "/standardagent", Symbol: "standardagent.NewApplication default", Available: true},
	{ID: "session.file", ComponentID: "session.store", Package: module + "/session", Symbol: "session.NewFileModule", Available: true, Configuration: []ConfigurationField{
		{Key: "session-directory", Description: "Absolute durable Session directory outside the Workspace", Required: true},
	}},
	{ID: "model.openaicompat.executor", ComponentID: "model.executor", Package: module + "/model/openaicompat", Symbol: "openaicompat.NewModule", Available: true, Dependencies: []string{"credential.resolver"}, Configuration: []ConfigurationField{
		{Key: "provider-key", Description: "Stable provider selection key", Required: true},
		{Key: "provider-url", Description: "Credential-free absolute OpenAI-compatible base URL", Required: true},
		{Key: "model-id", Description: "Provider model identifier", Required: true},
		{Key: "credential-ref", Description: "Non-secret late-bound credential reference", Required: true},
	}},
	{ID: "model.openaicompat.counter", ComponentID: "model.token-counter", Package: module + "/model/openaicompat", Symbol: "openaicompat.NewModule default", Available: true},
	{ID: "gateway.cli", ComponentID: "gateway.channel", Package: module + "/interaction/cli", Symbol: "cli.New", Available: true},
	{ID: "credential.encrypted-file", ComponentID: "credential.resolver", Package: module + "/credential", Symbol: "credential.NewEncryptedFileResolver", Available: true, Configuration: []ConfigurationField{
		{Key: "credential-file", Description: "Absolute encrypted credential file outside the Workspace", Required: true},
		{Key: "credential-key-environment", Description: "Environment variable containing the independent decryption key", Required: true},
	}},
	{ID: "workspace.local", ComponentID: "workspace.manager", Package: module + "/workspace/local", Symbol: "local.NewModule", Available: true, Configuration: []ConfigurationField{
		{Key: "workspace", Description: "Absolute local Workspace root", Required: true},
	}},
	{ID: "artifact.file", ComponentID: "artifact.store", Package: module + "/artifact/file", Symbol: "file.NewModule", Available: true, Configuration: []ConfigurationField{
		{Key: "artifact-directory", Description: "Absolute immutable Artifact directory outside the Workspace", Required: true},
	}},
	{ID: "tool.files", ComponentID: "tool", Package: module + "/tool/files", Symbol: "files.NewModule", Available: true, Dependencies: []string{"workspace.manager", "policy.guard"}, ToolKeys: []string{"file_read", "file_write", "file_edit"}},
	{ID: "tool.bash", ComponentID: "tool", Package: module + "/tool/bash", Symbol: "bash.NewModule", Available: true, Dependencies: []string{"workspace.manager", "policy.guard"}, ToolKeys: []string{"bash"}},
	{ID: "tool.session-history", ComponentID: "tool", Package: module + "/tool/sessionhistory", Symbol: "sessionhistory.NewModule", Available: true, Dependencies: []string{"session.store"}, ToolKeys: []string{"session_history"}},
	{ID: "policy.tool-rules", ComponentID: "policy.guard", Package: module + "/policy", Symbol: "policy.NewToolRuleGuard", Available: true, Dependencies: []string{"approval.service"}},
	{ID: "approval.configured", ComponentID: "approval.service", Package: module + "/policy", Symbol: "policy.ApprovalFunc", Available: true, Configuration: []ConfigurationField{
		{Key: "approval-environment", Description: "Environment variable explicitly enabling effectful actions", Required: true},
	}},
}, Presets: []Preset{
	{ID: "local-coding", Title: "Local coding Agent", Implementations: []string{
		"standard.loop", "session.file", "model.openaicompat.executor", "model.openaicompat.counter", "gateway.cli",
		"credential.encrypted-file", "workspace.local", "artifact.file", "tool.files", "tool.bash", "tool.session-history",
		"policy.tool-rules", "approval.configured",
	}, ToolKeys: []string{"bash", "file_read", "file_write", "file_edit", "session_history"}},
	{ID: "minimal-chat", Title: "Minimal chat Agent", Implementations: []string{
		"standard.loop", "session.file", "model.openaicompat.executor", "model.openaicompat.counter", "gateway.cli", "credential.encrypted-file",
	}},
}}

func init() {
	if err := standardCatalog.Validate(); err != nil {
		panic(err)
	}
}
