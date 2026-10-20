package collector

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/YuLaiZ/token-usage/internal/model"
)

// dshUsage 是一条模型调用的四桶 token；TotalTokens 用指针区分「缺失」与 0。
type dshUsage struct {
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	CacheReadTokens  int64  `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
	ReasoningTokens  int64  `json:"reasoningTokens"`
	TotalTokens      *int64 `json:"totalTokens"`
}

// dshSource 是消息或路由的 provider/model。
type dshSource struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// dshMessageData 是 assistant/message 与 assistant/attempt 的 data 子集。
type dshMessageData struct {
	Turn    int       `json:"turn"`
	Step    int       `json:"step"`
	Usage   *dshUsage `json:"usage"`
	Message *struct {
		ID     string     `json:"id"`
		Source *dshSource `json:"source"`
	} `json:"message"`
	Stream []struct {
		Chunk *struct {
			Type  string    `json:"type"`
			Usage *dshUsage `json:"usage"`
		} `json:"chunk"`
	} `json:"stream"`
}

// usage 先取 data.usage，否则取 stream 中最后一个 usage chunk。
func (d dshMessageData) usage() (dshUsage, bool) {
	if d.Usage != nil {
		return *d.Usage, true
	}
	for i := len(d.Stream) - 1; i >= 0; i-- {
		chunk := d.Stream[i].Chunk
		if chunk != nil && chunk.Type == "usage" && chunk.Usage != nil {
			return *chunk.Usage, true
		}
	}
	return dshUsage{}, false
}

type dshSlotKey struct {
	Turn    int
	Step    int
	Attempt int
}

type dshSlot struct {
	firstSeenTS int64
	sawNonzero  bool
	usage       dshUsage
	provider    string
	model       string
}

// parsedMessage 是缓存与归约层使用的消息：不存 Date，Date 在请求构造时
// 按进程固定时区由 FirstSeenTS 现算，避免跨时区缓存漂移。
type parsedMessage struct {
	FirstSeenTS         int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheWriteTokens    int64
	ReasoningTokens     int64
	TotalTokens         int64
	Model, Provider     string
	SessionID           string
	Directory           string
	Project             string
	Turn, Step, Attempt int
}

type parsedObsolete struct {
	ID        string
	SessionID string
	TS        int64
}

type parsedResult struct {
	Messages []parsedMessage
	Obsolete []parsedObsolete
	Sessions []model.Session
}

// buildDSHResult 把解码后的事件归约为消息、会话与零值撤销候选。
//
// 规则：
//   - 切点前的 message/attempt/retry/title 不参与；request/context 保留最后一个作为初始路由；
//   - attempt 用 retry-started 分段，段内替换、跨段累加；
//   - 每个槽只产一行；最终非零产 Message，最终 0 且曾非零产 Obsolete，其余不产。
func buildDSHResult(decoded *dshDecoded) parsedResult {
	var result parsedResult
	if decoded == nil || decoded.AllInherited || !decoded.HasUsageEvents {
		return result
	}

	session := model.Session{
		ID:        decoded.Header.ID,
		Client:    model.ClientDSH,
		Directory: decoded.Header.Cwd,
		Project:   projectBase(decoded.Header.Cwd),
		ParentID:  decoded.Header.ParentSession,
	}

	cut := decoded.Cut
	route := dshSource{}
	attempts := make(map[[2]int]int)
	slots := make(map[dshSlotKey]*dshSlot)
	ownUsage := false

	for _, event := range decoded.Events {
		if event.Seq <= cut {
			if event.Type == "request/context" {
				route = parseDSHRoute(event.Data, route)
			}
			continue
		}
		switch event.Type {
		case "request/context":
			route = parseDSHRoute(event.Data, route)
		case "session/title":
			var data struct {
				Title string `json:"title"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.Title != "" {
				session.Title = data.Title
			}
		case "llm/retry-started":
			var data struct {
				Turn int `json:"turn"`
				Step int `json:"step"`
			}
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			attempts[[2]int{data.Turn, data.Step}]++
		case "assistant/message", "assistant/attempt":
			var data dshMessageData
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			usage, ok := data.usage()
			if !ok {
				continue
			}
			ownUsage = true
			key := dshSlotKey{
				Turn:    data.Turn,
				Step:    data.Step,
				Attempt: attempts[[2]int{data.Turn, data.Step}],
			}
			slot := slots[key]
			if slot == nil {
				slot = &dshSlot{firstSeenTS: event.Time}
				slots[key] = slot
			}
			slot.usage = usage
			if dshUsageNonzero(usage) {
				slot.sawNonzero = true
			}
			if event.Type == "assistant/message" {
				if data.Message != nil && data.Message.Source != nil {
					slot.provider = data.Message.Source.Provider
					slot.model = data.Message.Source.Model
				}
			} else {
				slot.provider = route.Provider
				slot.model = route.Model
			}
		}
	}

	if !ownUsage {
		// 只有继承段、没有切点后用量事件：不产会话（空壳），也不能把父的会话带出来。
		return parsedResult{}
	}

	for key, slot := range slots {
		switch {
		case dshUsageNonzero(slot.usage):
			result.Messages = append(result.Messages, dshMessageFromSlot(session, key, slot))
		case slot.sawNonzero:
			result.Obsolete = append(result.Obsolete, parsedObsolete{
				ID:        dshMessageID(session.ID, key),
				SessionID: session.ID,
				TS:        slot.firstSeenTS,
			})
		}
	}

	sort.Slice(result.Messages, func(i, j int) bool {
		if result.Messages[i].Turn != result.Messages[j].Turn {
			return result.Messages[i].Turn < result.Messages[j].Turn
		}
		if result.Messages[i].Step != result.Messages[j].Step {
			return result.Messages[i].Step < result.Messages[j].Step
		}
		return result.Messages[i].Attempt < result.Messages[j].Attempt
	})
	if len(result.Messages) > 0 {
		session.FirstTS = result.Messages[0].FirstSeenTS
		session.LastTS = result.Messages[0].FirstSeenTS
		for _, message := range result.Messages[1:] {
			if message.FirstSeenTS < session.FirstTS {
				session.FirstTS = message.FirstSeenTS
			}
			if message.FirstSeenTS > session.LastTS {
				session.LastTS = message.FirstSeenTS
			}
		}
	}
	if len(result.Messages) == 0 && len(result.Obsolete) == 0 {
		// 只有全零 attempt、没有任何可计费样本：不产空会话（first/last 无意义）。
		return parsedResult{}
	}
	result.Sessions = append(result.Sessions, session)
	return result
}

func parseDSHRoute(data json.RawMessage, fallback dshSource) dshSource {
	var route dshSource
	if json.Unmarshal(data, &route) != nil {
		return fallback
	}
	if route.Provider == "" && route.Model == "" {
		return fallback
	}
	return route
}

func dshUsageNonzero(usage dshUsage) bool {
	if usage.TotalTokens != nil && *usage.TotalTokens != 0 {
		return true
	}
	return usage.InputTokens != 0 || usage.OutputTokens != 0 ||
		usage.CacheReadTokens != 0 || usage.CacheWriteTokens != 0 ||
		usage.ReasoningTokens != 0
}

func dshUsageTotal(usage dshUsage) int64 {
	if usage.TotalTokens != nil {
		return *usage.TotalTokens
	}
	return usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
}

func dshMessageID(sessionID string, key dshSlotKey) string {
	return fmt.Sprintf("%s:%d:%d:%d", sessionID, key.Turn, key.Step, key.Attempt)
}

func dshMessageFromSlot(session model.Session, key dshSlotKey, slot *dshSlot) parsedMessage {
	return parsedMessage{
		FirstSeenTS:      slot.firstSeenTS,
		InputTokens:      slot.usage.InputTokens,
		OutputTokens:     slot.usage.OutputTokens,
		CacheReadTokens:  slot.usage.CacheReadTokens,
		CacheWriteTokens: slot.usage.CacheWriteTokens,
		ReasoningTokens:  slot.usage.ReasoningTokens,
		TotalTokens:      dshUsageTotal(slot.usage),
		Model:            slot.model,
		Provider:         slot.provider,
		SessionID:        session.ID,
		Directory:        session.Directory,
		Project:          session.Project,
		Turn:             key.Turn,
		Step:             key.Step,
		Attempt:          key.Attempt,
	}
}

// toModelMessage 在请求构造时把缓存中的 TS 转成 Date。
func toModelMessage(message parsedMessage) model.Message {
	return model.Message{
		ID:                dshMessageID(message.SessionID, dshSlotKey{message.Turn, message.Step, message.Attempt}),
		SessionID:         message.SessionID,
		Client:            model.ClientDSH,
		Date:              tsMsToDate(message.FirstSeenTS),
		TS:                message.FirstSeenTS,
		Model:             message.Model,
		Provider:          dshProviderDisplay(message.Provider),
		Directory:         message.Directory,
		Project:           message.Project,
		InputTokens:       message.InputTokens,
		FreshInputTokens:  message.InputTokens,
		OutputTokens:      message.OutputTokens,
		CacheReadTokens:   message.CacheReadTokens,
		CacheCreateTokens: message.CacheWriteTokens,
		ReasoningTokens:   message.ReasoningTokens,
		TotalTokens:       message.TotalTokens,
	}
}

func toModelMessages(messages []parsedMessage) []model.Message {
	out := make([]model.Message, 0, len(messages))
	for _, message := range messages {
		out = append(out, toModelMessage(message))
	}
	return out
}

// toDSHObsolete 把归约层的撤销候选转成落库计划；Date 在此按进程时区现算。
func toDSHObsolete(items []parsedObsolete) []DSHObsoleteMessage {
	out := make([]DSHObsoleteMessage, 0, len(items))
	for _, item := range items {
		out = append(out, DSHObsoleteMessage{
			ID:        item.ID,
			SessionID: item.SessionID,
			TS:        item.TS,
			Date:      tsMsToDate(item.TS),
		})
	}
	return out
}

// dshProviderDisplayNames 是落库用的 provider 显示名；未登记的 provider 原样保留。
var dshProviderDisplayNames = map[string]string{
	"deepseek-account": "DeepSeek Account",
}

func dshProviderDisplay(provider string) string {
	if display, ok := dshProviderDisplayNames[provider]; ok {
		return display
	}
	return provider
}
