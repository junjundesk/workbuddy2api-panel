// accounts_batch.go 号池批量运维：多选删除账号（出池 + 删 auths/ 下的凭证文件）。
//
// 与单号 remove 同一口径（pool.Remove → groups.RemoveMember → os.Remove），
// 差别只在一次处理多个 uid 并回逐条结果：前端多选后要知道"哪几个真的删了、
// 哪几个失败及原因"，而不是一个笼统的 ok。
package panel

import (
	"log"
	"net/http"
	"os"
	"strings"
)

// accountsRemoveBatchMax 单次批量删除上限：防误触把整个号池一次删空
// （前端全选按钮受此约束，后端仍独立校验）。
const accountsRemoveBatchMax = 500

// accountsRemoveBatch POST /panel/api/accounts/remove_batch {"uids":[...]}。
// 每个 uid 独立处理，互不影响；返回 removed / failed / file_errors 三份清单。
func (p *Panel) accountsRemoveBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	seen := make(map[string]bool, len(req.UIDs))
	uids := make([]string, 0, len(req.UIDs))
	for _, uid := range req.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		writeErr(w, http.StatusBadRequest, "uids is required")
		return
	}
	if len(uids) > accountsRemoveBatchMax {
		writeErr(w, http.StatusBadRequest, "too many uids in one request (max 500)")
		return
	}

	type failure struct {
		UID   string `json:"uid"`
		Error string `json:"error"`
	}
	removed := make([]string, 0, len(uids))
	failed := make([]failure, 0)
	fileErrors := make([]failure, 0)

	for _, uid := range uids {
		a := p.cfg.Pool.Remove(uid)
		if a == nil {
			failed = append(failed, failure{UID: uid, Error: "account not found"})
			continue
		}
		if p.cfg.Groups != nil {
			if gid := p.cfg.Groups.GroupOf(uid); gid != "" {
				if err := p.cfg.Groups.RemoveMember(gid, uid); err != nil {
					log.Printf("panel: 批量删除 uid=%s 移出分组 group=%s: %v", uid, gid, err)
				}
			}
		}
		if a.FilePath != "" {
			if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
				// 出池已生效（池状态已落盘），文件删不掉只影响重启后的重新装载：
				// 记进清单让运维手动处理，不回滚整批。
				fileErrors = append(fileErrors, failure{UID: uid, Error: err.Error()})
			}
		}
		removed = append(removed, uid)
	}

	log.Printf("panel: 批量删除账号：请求 %d，已删除 %d，失败 %d，凭证文件失败 %d",
		len(uids), len(removed), len(failed), len(fileErrors))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"removed":     removed,
		"failed":      failed,
		"file_errors": fileErrors,
	})
}
