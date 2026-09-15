package bodymigrate

import (
	"strings"
	"testing"
)

// orderOf reads one legacy construct from src and plans its order.
func orderOf(t *testing.T, src string) ([]*lstmt, orderPlan) {
	t.Helper()
	cs := findConstructs(src)
	if len(cs) != 1 {
		t.Fatalf("want one construct, found %d", len(cs))
	}
	lb, err := readConstructBody("t.memql", src, cs[0])
	if err != nil {
		t.Fatal(err)
	}
	return lb.stmts, planOrder(lb.stmts, NewIndex())
}

func names(stmts []*lstmt, order []int) string {
	var out []string
	for _, i := range order {
		out = append(out, label(stmts[i]))
	}
	return strings.Join(out, ",")
}

func TestBodiesOrderPolicy(t *testing.T) {
	t.Run("a statement reading a later step moves below it, and says so", func(t *testing.T) {
		// a reads c, written after it: the compiler runs the first ready step
		// in source order, so b, then c, then a.
		stmts, plan := orderOf(t, `
logic l {
  body {
    a := builtin one(v: c.first())
    b := builtin two()
    c := builtin three()
    return c
  }
}`)
		if got := names(stmts, plan.order); got != "b,c,a,the return" {
			t.Fatalf("order = %s, want b,c,a,the return", got)
		}
		if len(plan.comments[0]) != 1 || !strings.Contains(plan.comments[0][0], "moved below c -- the engine ran this after it") {
			t.Fatalf("a's comment = %q, want one naming the move below c", plan.comments[0])
		}
		for _, i := range []int{1, 2} {
			if len(plan.comments[i]) != 0 {
				t.Fatalf("%s kept its place; its comments = %q", label(stmts[i]), plan.comments[i])
			}
		}
	})
	t.Run("every reference is an edge: an undotted read keeps the source order, uncommented", func(t *testing.T) {
		// The compiler before epic 2's flip saw no undotted name and ran seen
		// before probe; today's compiler sees it.
		stmts, plan := orderOf(t, `
logic l {
  body {
    r := builtin one()
    probe := r.first().x ?? ""
    seen := probe == "" ? false : true
    return seen
  }
}`)
		if got := names(stmts, plan.order); got != "r,probe,seen,the return" {
			t.Fatalf("order = %s, want the source order", got)
		}
		for i, c := range plan.comments {
			if len(c) > 0 {
				t.Fatalf("statement %d carries %q; nothing moved", i, c)
			}
		}
	})
	t.Run("steps freed together run in the order written, uncommented", func(t *testing.T) {
		stmts, plan := orderOf(t, `
@trigger(event="x")
automation a {
  step d {
    builtin decide()
  }
  step m1 {
    mutation write1 { v: d.result }
  }
  step m2 {
    mutation write2 { v: d.result }
  }
}`)
		if got := names(stmts, plan.order); got != "d,m1,m2" {
			t.Fatalf("order = %s", got)
		}
		for i, c := range plan.comments {
			if len(c) > 0 {
				t.Fatalf("statement %d carries %q; the order is the one written", i, c)
			}
		}
	})
}
