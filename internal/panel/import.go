package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/junjundesk/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 导入 JSON 的单个账号（归一后的中间形）。兼容三类来源：
//   - cockpit tools 导出的扁平 snake_case；
//   - 面板「导出所选账号」的扁平 camelCase（*CC 别名，与嵌套形共用）；
//   - 插件 OAuth 落盘的嵌套形 {"auth":{...},"account":{...}}（= auths/workbuddy-*.json
//     原文件，见 decodeImportAccount）。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`

	// 面板「导出所选账号」格式（workbuddy-accounts，camelCase 字段）的别名；
	// 嵌套形导入时 EnterpriseIDCC 承载 account.enterpriseId。
	Realm          string `json:"realm"`
	AccessTokenCC  string `json:"accessToken"`
	RefreshTokenCC string `json:"refreshToken"`
	ExpiresAtCC    int64  `json:"expiresAt"`
	EnterpriseIDCC string `json:"enterpriseId"`

	// DeviceToken 设备风控 Token（nested auth 文件顶层 device_token；手写扁平形也可写）。
	// 导入后随 auth.SaveAtomic 写回顶层 device_token，否则导入即丢该账号的风控标识。
	DeviceToken string `json:"device_token"`
}

// decodeImportAccounts 兼容三种输入容器：JSON 数组 [...]、{"accounts":[...]}、单个对象 {...}。
//
// 容器内每个账号项支持两种形态（同一文件里可以混用）：
//   - 扁平形：cockpit tools 导出（snake_case）或面板导出（camelCase）；
//   - 嵌套形：插件 OAuth 落盘的 auth 文件原文 {"auth":{...},"account":{...}}，
//     即 auths/workbuddy-*.json 的内容（见 decodeImportAccount）。
//
// 归一到 cockpitAccount：camelCase 别名落到 *CC 字段，expires_at 统一为秒。
func decodeImportAccounts(raw []byte) ([]cockpitAccount, error) {
	items, err := splitImportItems(raw)
	if err != nil {
		return nil, err
	}
	accounts := make([]cockpitAccount, 0, len(items))
	for _, item := range items {
		a, err := decodeImportAccount(item)
		if err != nil {
			return nil, err
		}
		normalizeImportAccount(&a)
		accounts = append(accounts, a)
	}
	return accounts, nil
}

// splitImportItems 把三种输入容器拆成逐账号的原始 JSON 片段。
// {"accounts":[...]} 的 accounts 为 null/缺失时，整体按单个扁平账号对象处理（旧行为）。
func splitImportItems(raw []byte) ([]json.RawMessage, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
	switch {
	case strings.HasPrefix(trimmed, "["):
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, err
		}
		return items, nil
	case strings.HasPrefix(trimmed, "{"):
		var wrap struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrap); err != nil {
			return nil, err
		}
		if len(wrap.Accounts) > 0 && string(wrap.Accounts) != "null" {
			var items []json.RawMessage
			if err := json.Unmarshal(wrap.Accounts, &items); err != nil {
				return nil, err
			}
			return items, nil
		}
		return []json.RawMessage{json.RawMessage(trimmed)}, nil
	default:
		return nil, fmt.Errorf("expected JSON array or object")
	}
}

// decodeImportAccount 解码单个账号项。带 "auth" 键的按嵌套形（插件 OAuth / auths 文件）解，
// 字段与 auth.Parse 一一对应：auth.accessToken/refreshToken/expiresAt/domain/realm、
// account.uid/enterpriseId/nickname、顶层 device_token。
//
// 为何手写宽松解码而不直接调 auth.Parse：auth.Parse 在缺 accessToken 时报错；导入场景下
// 这类坏项应逐条记 skipped（还能带回 uid 便于排查），而不是让整份文件 400、连带丢掉同文件
// 里的好账号。parse 成功的账号最终仍经 auth.SaveAtomic 落成同一种嵌套形。
func decodeImportAccount(raw json.RawMessage) (cockpitAccount, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return cockpitAccount{}, err
	}
	// 仅当 auth 是 JSON 对象才按嵌套形解：扁平项里可能恰好有名为 auth 的其它字段，
	// 误判会让整份文件 400（旧行为下它只是被忽略）。
	if rawAuth, ok := probe["auth"]; ok && strings.HasPrefix(strings.TrimSpace(string(rawAuth)), "{") {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return cockpitAccount{}, err
		}
		return cockpitAccount{
			UID:            n.Account.UID,
			Nickname:       n.Account.Nickname,
			AccessToken:    n.Auth.AccessToken,
			RefreshToken:   n.Auth.RefreshToken,
			ExpiresAt:      n.Auth.ExpiresAt,
			Domain:         n.Auth.Domain,
			Realm:          n.Auth.Realm,
			EnterpriseIDCC: n.Account.EnterpriseID,
			DeviceToken:    n.DeviceToken,
		}, nil
	}
	var one cockpitAccount
	if err := json.Unmarshal(raw, &one); err != nil {
		return cockpitAccount{}, err
	}
	return one, nil
}

// normalizeImportAccount camelCase 别名归一到 snake_case 字段，expires_at 统一为秒
// （cockpit 用毫秒、面板导出与 auth 文件用秒：> 1e12 视为毫秒）。
func normalizeImportAccount(a *cockpitAccount) {
	if a.AccessToken == "" {
		a.AccessToken = a.AccessTokenCC
	}
	if a.RefreshToken == "" {
		a.RefreshToken = a.RefreshTokenCC
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = a.ExpiresAtCC
	}
	if a.ExpiresAt > 1_000_000_000_000 {
		a.ExpiresAt /= 1000
	}
}

// importCockpit 接收账号 JSON（cockpit tools 导出 / 面板导出 / 插件 OAuth auths 文件原样），
// 批量导入账号到池中：解码见 decodeImportAccounts，逐条落盘 auths/ 并热加载进池。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	groupID := strings.TrimSpace(r.FormValue("group"))
	if groupID != "" {
		if p.cfg.Groups == nil {
			writeErr(w, http.StatusBadRequest, "groups not available")
			return
		}
		if _, ok := p.cfg.Groups.Get(groupID); !ok {
			writeErr(w, http.StatusBadRequest, "unknown group")
			return
		}
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	accounts, err := decodeImportAccounts(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			if uid == "" && at == "" && rt == "" {
				// 三个关键字段全空：多半是 JSON 形态没被识别。给出可操作提示，而不是
				// 一条 uid=/id= 全空的废话（用户导入插件 auth 文件被全量跳过时就是这种情况）。
				errs = append(errs, "unrecognized account JSON: want uid + accessToken/access_token, or a plugin auth file with auth.accessToken")
			} else {
				errs = append(errs, fmt.Sprintf("missing required fields (uid=%s id=%s)", uid, acc.ID))
			}
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		if groupID != "" && p.cfg.Groups != nil {
			if existing := p.cfg.Groups.GroupOf(uid); existing != "" && existing != groupID {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: already in another group (%s); move it explicitly first", uid, existing))
				continue
			}
		}

		// 显式 realm（面板导出带）优先，否则按 domain 推断。
		realm := auth.ResolveRealm(acc.Realm, acc.Domain)
		if realm != "cn" && realm != "global" {
			realm = auth.ResolveRealm("", acc.Domain)
		}

		// decodeImportAccounts 已统一为秒。
		expiresAt := acc.ExpiresAt
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		// 嵌套形（auths 文件原样导入）带来的企业 ID / 设备 token 要一并落盘：
		// EnterpriseID 参与 auth.IsEnterprise 的签到/成长任务门控，DeviceToken 参与
		// X-Device-Token 注入；丢了它们等于导入后账号行为与原 auth 文件不一致。
		enterpriseID := strings.TrimSpace(acc.EnterpriseIDCC)
		deviceToken := strings.TrimSpace(acc.DeviceToken)

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			EnterpriseID: enterpriseID,
			Nickname:     nickname,
			DeviceToken:  deviceToken,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		if groupID != "" && p.cfg.Groups != nil {
			if err := p.cfg.Groups.AddMember(groupID, uid); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: add group failed: %v", uid, err))
				continue
			}
			p.cfg.Pool.SetGroup(uid, groupID)
		}
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}

		imported++
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
