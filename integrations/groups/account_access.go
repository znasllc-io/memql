package groups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

// The production adapter routes existing group doors through the engine's
// transactional account policy. This preserves their public API without
// allowing global user roles to bypass a person's organization role.
type accountAccessEngine interface {
	ManageAccount(context.Context, string, map[string]any) (map[string]any, error)
}

func (a engineAdapter) ManageAccount(ctx context.Context, accountID string, args map[string]any) (map[string]any, error) {
	call := func(name string, args map[string]any) (map[string]any, error) {
		keys := []string{}
		for key := range args {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := []string{}
		for _, key := range keys {
			value, err := json.Marshal(args[key])
			if err != nil {
				return nil, err
			}
			parts = append(parts, key+": "+string(value))
		}
		result, err := a.engine.Execute(ctx, fmt.Sprintf("builtin %s(%s)", name, strings.Join(parts, ", ")))
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 {
			return nil, errors.New("account access returned no answer")
		}
		row := rows[0]
		if payload, ok := row["payload"].(map[string]any); ok {
			row = payload
		}
		return row, nil
	}
	if args != nil && args["operation"] == "leave" {
		args["accountId"] = accountID
		return call("accountAccessUpdate", args)
	}
	state, err := call("accountAccess", map[string]any{"accountId": accountID})
	if err != nil {
		return nil, err
	}
	if args == nil {
		return state, nil
	}
	args["accountId"], args["revision"] = accountID, state["revision"]
	return call("accountAccessUpdate", args)
}
