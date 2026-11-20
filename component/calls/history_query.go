package calls

import (
	"net/url"
	"strconv"
)

// ParseHistoryQuery is the closed, bounded GET allowlist shared by edge and BFF.
// Query strings never carry credentials; every other calls route still drops
// them. Reject duplicate/unknown names rather than choosing one interpretation.
func ParseHistoryQuery(raw string) (HistoryRequest, error) {
	var req HistoryRequest
	if len(raw) > 6144 {
		return req, ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return req, ErrInvalid
	}
	for key, list := range values {
		if len(list) != 1 {
			return req, ErrInvalid
		}
		value := list[0]
		switch key {
		case "archive":
			if value != "0" && value != "1" {
				return req, ErrInvalid
			}
			req.Archive = value == "1"
		case "limit":
			req.Limit, err = strconv.Atoi(value)
			if err != nil || req.Limit < 1 || req.Limit > 50 {
				return req, ErrInvalid
			}
		case "period":
			req.Period, err = strconv.Atoi(value)
			if err != nil || (req.Period != 7 && req.Period != 30 && req.Period != 90) {
				return req, ErrInvalid
			}
		case "cursor":
			if len(value) > 4096 {
				return req, ErrInvalid
			}
			req.Cursor = value
		case "search":
			if len(value) > 256 {
				return req, ErrInvalid
			}
			req.Search = value
		case "role":
			if value != "host" && value != "guest" {
				return req, ErrInvalid
			}
			req.Role = value
		case "status":
			if value != "active" && value != "ended" && value != "scheduled" && value != "cancelled" {
				return req, ErrInvalid
			}
			req.Status = value
		case "from":
			if len(value) > 40 {
				return req, ErrInvalid
			}
			req.From = value
		case "to":
			if len(value) > 40 {
				return req, ErrInvalid
			}
			req.To = value
		default:
			return req, ErrInvalid
		}
	}
	return req, nil
}
