package management

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

const financeRead = "cost_centers:read_all cost_centers:modify cost_centers:create"
const financeModify = "cost_centers:modify"

func userID(r request) int64 { return access.From(r.Context()).ID }
func privateCenter(tx *sql.Tx, uid int64) error {
	if uid <= 0 {
		return nil
	}
	n, e := count(tx, "SELECT count(*) FROM cost_centers WHERE is_private AND owner_user_id=?", uid)
	if e != nil || n > 0 {
		return e
	}
	u, e := get(tx, "users", uid)
	if e != nil {
		return e
	}
	_, e = insert(tx, "cost_centers", row{"name": str(u["username"]), "code": "USER-" + strconv.FormatInt(uid, 10), "is_active": true, "is_private": true, "owner_user_id": uid})
	return e
}
func (s *Service) finance(m *http.ServeMux) {
	const base = "finance/cost-centers"
	const mine = "cost_centers:read_own queue:create"
	s.route(m, "GET", base, financeRead, func(r request) (any, error) { return centerList(r, false) })
	s.route(m, "GET", base+"/mine", mine, func(r request) (any, error) { return centerList(r, true) })
	s.route(m, "POST", base, "cost_centers:create", func(r request) (any, error) {
		d, e := r.input("CostCenterCreateRequest")
		if e != nil {
			return nil, e
		}
		if d["monthly_budget"] != nil {
			d["total_budget"] = nil
		}
		var code [8]byte
		if _, e = rand.Read(code[:]); e != nil {
			return nil, e
		}
		d["code"] = "CC-" + hex.EncodeToString(code[:])
		d["is_private"] = false
		id, e := insert(r.tx, "cost_centers", d)
		if e != nil {
			return nil, e
		}
		return centerView(r, id, false)
	})
	s.route(m, "GET", base+"/{id}", financeRead, func(r request) (any, error) { return centerView(r, r.id(), true) })
	for suffix, model := range map[string]string{"": "CostCenterUpdateRequest", "/budgets": "CostCenterBudgetUpdateRequest"} {
		s.route(m, "PATCH", base+"/{id}"+suffix, financeModify, func(r request) (any, error) {
			d, e := r.input(model)
			if e != nil {
				return nil, e
			}
			if suffix == "/budgets" {
				if d["monthly_budget"] != nil {
					d["total_budget"] = nil
				} else {
					d["monthly_budget"] = nil
					defaultTo(d, "total_budget", nil)
				}
			}
			if suffix == "" {
				c, e := get(r.tx, "cost_centers", r.id())
				if e != nil {
					return nil, e
				}
				if truth(c["is_private"]) && d["is_active"] == false {
					return nil, bad("Private cost centers must remain active")
				}
			}
			d["updated_at"] = now()
			if e = update(r.tx, "cost_centers", r.id(), d); e != nil {
				return nil, e
			}
			return centerView(r, r.id(), false)
		})
	}
	s.route(m, "DELETE", base+"/{id}", financeModify, func(r request) (any, error) {
		c, e := get(r.tx, "cost_centers", r.id())
		if e != nil {
			return nil, e
		}
		if truth(c["is_private"]) {
			return nil, bad("Cannot delete a private cost center")
		}
		n, e := count(r.tx, "SELECT (SELECT count(*) FROM wallet_transactions WHERE cost_center_id=? AND NOT is_voided)+(SELECT count(*) FROM budget_reservations WHERE cost_center_id=? AND status='active')+(SELECT count(*) FROM print_queue WHERE cost_center_id=? AND status IN ('pending','printing','dispatching'))", r.id(), r.id(), r.id())
		if e != nil {
			return nil, e
		}
		if n > 0 {
			return nil, bad("Cost center has financial commitments or history")
		}
		return row{"status": "success"}, affect(r.tx, "DELETE FROM cost_centers WHERE id=?", r.id())
	})
	s.route(m, "POST", base+"/{id}/members", financeModify, func(r request) (any, error) {
		c, e := get(r.tx, "cost_centers", r.id())
		if e != nil {
			return nil, e
		}
		if truth(c["is_private"]) {
			return nil, bad("Private cost centers cannot have members")
		}
		d, e := r.input("CostCenterMemberRequest")
		if e != nil {
			return nil, e
		}
		_, e = r.tx.Exec("INSERT INTO cost_center_members(cost_center_id,user_id,can_print) VALUES(?,?,?) ON CONFLICT(cost_center_id,user_id) DO UPDATE SET can_print=excluded.can_print", r.id(), d["user_id"], d["can_print"])
		if e != nil {
			return nil, e
		}
		return one(r.tx, "SELECT * FROM cost_center_members WHERE cost_center_id=? AND user_id=?", r.id(), d["user_id"])
	})
	s.route(m, "DELETE", base+"/{id}/members/{user}", financeModify, func(r request) (any, error) {
		return row{"status": "success"}, affect(r.tx, "DELETE FROM cost_center_members WHERE cost_center_id=? AND user_id=?", r.id(), r.PathValue("user"))
	})
	s.route(m, "GET", "finance/me/balance", "cost_centers:read_own", func(r request) (any, error) { return balance(r, userID(r)) })
	s.route(m, "GET", "finance/users/{id}/balance", financeRead, func(r request) (any, error) { return balance(r, r.id()) })
	s.route(m, "GET", "finance/me/transactions", "cost_centers:read_own", func(r request) (any, error) { return transactions(r, userID(r), true) })
	s.route(m, "GET", "finance/transactions", financeRead, func(r request) (any, error) { return transactions(r, 0, false) })
	s.route(m, "GET", "finance/users/{id}/transactions", financeRead, func(r request) (any, error) {
		v, e := transactions(r, r.id(), true)
		if e != nil {
			return nil, e
		}
		return v["items"], nil
	})
	for _, op := range []string{"deposit", "withdraw"} {
		s.route(m, "POST", "finance/users/{id}/"+op, financeModify, func(r request) (any, error) {
			d, e := r.input("WalletAdjustmentRequest")
			if e != nil {
				return nil, e
			}
			if _, e = get(r.tx, "users", r.id()); e != nil {
				return nil, e
			}
			if num(d["amount"]) <= 0 {
				return nil, invalid("A positive amount is required")
			}
			if op == "withdraw" {
				d["amount"] = -num(d["amount"])
			}
			d["transaction_type"] = op
			d["user_id"] = r.id()
			d["created_by_user_id"] = nullID(userID(r))
			v, e := writeLedger(r.tx, d)
			if e != nil {
				return nil, e
			}
			b, e := balance(r, r.id())
			return row{"transaction": v, "balance": b}, e
		})
	}
	s.route(m, "POST", "finance/transactions/manual", financeModify, func(r request) (any, error) {
		d, e := r.input("ManualPrintRequest")
		if e != nil {
			return nil, e
		}
		d["amount"] = -num(d["amount"])
		d["transaction_type"] = "print_charge"
		d["created_by_user_id"] = nullID(userID(r))
		return writeLedger(r.tx, d)
	})
	s.route(m, "PATCH", "finance/transactions/{id}", financeModify, func(r request) (any, error) {
		d, e := r.input("TransactionEditRequest")
		if e != nil {
			return nil, e
		}
		old, e := one(r.tx, "SELECT * FROM wallet_transactions WHERE id=? AND NOT is_voided", r.id())
		if e != nil {
			return nil, e
		}
		if v, ok := d["amount"]; ok {
			if v == nil || num(v) <= 0 {
				return nil, invalid("A positive amount is required")
			}
			a := cents(num(v))
			if num(old["amount"]) < 0 {
				a = -a
			}
			d["amount"] = a
		}
		if e = update(r.tx, "wallet_transactions", r.id(), d); e != nil {
			return nil, e
		}
		if e = rebuildLedger(r.tx); e != nil {
			return nil, e
		}
		return get(r.tx, "wallet_transactions", r.id())
	})
	s.route(m, "DELETE", "finance/transactions/{id}", financeModify, func(r request) (any, error) {
		v, e := one(r.tx, "SELECT * FROM wallet_transactions WHERE id=? AND NOT is_voided", r.id())
		if e != nil {
			return nil, e
		}
		if v["print_run_id"] != nil {
			e = update(r.tx, "wallet_transactions", r.id(), row{"is_voided": true})
		} else {
			e = affect(r.tx, "DELETE FROM wallet_transactions WHERE id=?", r.id())
		}
		if e != nil {
			return nil, e
		}
		return row{"status": "success"}, rebuildLedger(r.tx)
	})
	s.route(m, "POST", "finance/rebuild-balance-ledger", financeModify, func(r request) (any, error) { return row{"status": "success"}, rebuildLedger(r.tx) })
}
func centerView(r request, id int64, members bool) (row, error) {
	c, e := get(r.tx, "cost_centers", id)
	if e != nil {
		return nil, e
	}
	ok, e := canBill(r.tx, c, userID(r), access.From(r.Context()).Admin)
	if e != nil {
		return nil, e
	}
	v, e := centerSummary(r.Context(), r.tx, c, ok)
	if e == nil && members {
		v["members"], e = list(r.tx, "SELECT * FROM cost_center_members WHERE cost_center_id=? ORDER BY id", id)
	}
	return v, e
}
func centerList(r request, mine bool) (any, error) {
	uid := userID(r)
	if uid > 0 {
		if e := privateCenter(r.tx, uid); e != nil {
			return nil, e
		}
	}
	q := "SELECT * FROM cost_centers"
	var args []any
	if mine {
		q += " WHERE is_active"
		if !access.From(r.Context()).Admin {
			q += " AND (owner_user_id=? OR EXISTS(SELECT 1 FROM cost_center_members m WHERE m.cost_center_id=cost_centers.id AND m.user_id=?))"
			args = append(args, uid, uid)
		}
	}
	rows, e := list(r.tx, q+" ORDER BY is_private DESC,name", args...)
	if e != nil {
		return nil, e
	}
	for i, c := range rows {
		allowed, e := canBill(r.tx, c, uid, access.From(r.Context()).Admin)
		if e != nil {
			return nil, e
		}
		rows[i], e = centerSummary(r.Context(), r.tx, c, allowed)
		if e != nil {
			return nil, e
		}
	}
	return rows, nil
}
func balance(r request, uid int64) (row, error) {
	if uid == 0 {
		return nil, &problem{401, "A user account is required"}
	}
	if _, e := get(r.tx, "users", uid); e != nil {
		return nil, e
	}
	b, e := one(r.tx, "SELECT balance,updated_at,user_id FROM user_wallets WHERE user_id=?", uid)
	if e == sql.ErrNoRows {
		b = row{"balance": 0, "updated_at": nil, "user_id": uid}
		e = nil
	}
	if e == nil {
		b["currency"] = setting(r.Context(), r.tx, "currency", "USD")
	}
	return b, e
}
func transactions(r request, uid int64, personal bool) (row, error) {
	filters := []string{"t.is_voided=0"}
	args := []any{}
	if personal {
		if uid == 0 {
			return nil, &problem{401, "A user account is required"}
		}
		filters = append(filters, "t.user_id=?")
		args = append(args, uid)
	}
	q := r.URL.Query()
	for _, key := range []string{"cost_center_id", "transaction_type", "user_id"} {
		if q.Get(key) != "" {
			filters = append(filters, "t."+key+"=?")
			args = append(args, q.Get(key))
		}
	}
	where := " FROM wallet_transactions t WHERE " + strings.Join(filters, " AND ")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	offset, _ := strconv.Atoi(q.Get("offset"))
	offset = max(offset, 0)
	n, e := count(r.tx, "SELECT count(*)"+where, args...)
	if e != nil {
		return nil, e
	}
	items, e := list(r.tx, "SELECT t.*"+where+" ORDER BY t.created_at DESC,t.id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	return row{"items": items, "limit": limit, "offset": offset, "total": n}, e
}
