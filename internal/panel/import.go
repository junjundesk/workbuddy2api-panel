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

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
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

	// 面板「导出所选账号」格式（workbuddy-accounts，camelCase 字段）的别名。
	Realm          string `json:"realm"`
	AccessTokenCC  string `json:"accessToken"`
	RefreshTokenCC string `json:"refreshToken"`
	ExpiresAtCC    int64  `json:"expiresAt"`
	EnterpriseIDCC string `json:"enterpriseId"`
}

// decodeImportAccounts 兼容三种输入：
//   - cockpit 导出的裸数组 [...]（snake_case）
//   - 面板导出的 {"format":"workbuddy-accounts","accounts":[...]}（camelCase）
//   - 单个账号对象 {...}
//
// camelCase 字段归一到 snake_case 字段；expires_at 统一转成秒。
func decodeImportAccounts(raw []byte) ([]cockpitAccount, error) {
	var accounts []cockpitAccount
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
	switch {
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal([]byte(trimmed), &accounts); err != nil {
			return nil, err
		}
	case strings.HasPrefix(trimmed, "{"):
		var wrap struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrap); err != nil {
			return nil, err
		}
		if len(wrap.Accounts) > 0 && string(wrap.Accounts) != "null" {
			if err := json.Unmarshal(wrap.Accounts, &accounts); err != nil {
				return nil, err
			}
		} else {
			var one cockpitAccount
			if err := json.Unmarshal([]byte(trimmed), &one); err != nil {
				return nil, err
			}
			accounts = []cockpitAccount{one}
		}
	default:
		return nil, fmt.Errorf("expected JSON array or object")
	}
	for i := range accounts {
		a := &accounts[i]
		if a.AccessToken == "" {
			a.AccessToken = a.AccessTokenCC
		}
		if a.RefreshToken == "" {
			a.RefreshToken = a.RefreshTokenCC
		}
		if a.ExpiresAt == 0 {
			a.ExpiresAt = a.ExpiresAtCC
		}
		// cockpit 用毫秒，面板导出用秒：> 1e12 视为毫秒。
		if a.ExpiresAt > 1_000_000_000_000 {
			a.ExpiresAt /= 1000
		}
	}
	return accounts, nil
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
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
			errs = append(errs, fmt.Sprintf("missing required fields (uid=%s id=%s)", uid, acc.ID))
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

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
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
