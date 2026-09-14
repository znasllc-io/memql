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
	t.Run("today respects every reference: write today's order", func(t *testing.T) {
		// b waits for a through a dotted reference the compiler saw; c was free
		// from the start, so the engine ran it before b.
		stmts, plan := orderOf(t, `
logic l {
  body {
    a := builtin one()
    b := builtin two(v: a.first())
    c := builtin three()
    return c
  }
}`)
		if got := names(stmts, plan.order); got != "a,c,b,the return" {
			t.Fatalf("order = %s, want a,c,b,the return", got)
		}
		// b is the statement the engine delayed: it waited for a, and Kahn's
		// queue let c, free from the start, run first.
		if len(plan.comments[1]) != 1 || !strings.Contains(plan.comments[1][0], "moved below c -- the engine ran this after it") {
			t.Fatalf("b's comment = %q, want one naming the move below c", plan.comments[1])
		}
		if len(plan.comments[2]) != 0 {
			t.Fatalf("c kept its place relative to the rest; its comments = %q", plan.comments[2])
		}
	})
	t.Run("today read a name unbound: write the source order and say so", func(t *testing.T) {
		stmts, plan := orderOf(t, `
logic l {
  body {
    r := builtin one()
    probe := coalesce(r.first().x, "")
    seen := cond(probe == "", false, true)
    return seen
  }
}`)
		if got := names(stmts, plan.order); got != "r,probe,seen,the return" {
			t.Fatalf("order = %s, want the source order", got)
		}
		if len(plan.comments[2]) != 1 || !strings.Contains(plan.comments[2][0], "before probe, which it reads, so it read nothing") {
			t.Fatalf("seen's comment = %q", plan.comments[2])
		}
		if len(plan.comments[1]) != 0 {
			t.Fatalf("probe moved nowhere and read nothing early; its comments = %q", plan.comments[1])
		}
	})
	t.Run("a tie between side effects is named; a tie between values is not", func(t *testing.T) {
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
		if len(plan.comments[2]) != 1 || !strings.Contains(plan.comments[2][0], "in no fixed order") {
			t.Fatalf("m2's comment = %q, want the tie named", plan.comments[2])
		}
		_, plan = orderOf(t, `
logic l {
  body {
    d := builtin decide()
    x := d.first()
    y := d.last()
    return x
  }
}`)
		for i, c := range plan.comments {
			if len(c) > 0 {
				t.Fatalf("statement %d carries %q; a tie between values changes nothing", i, c)
			}
		}
	})
}
