package id

import (
	"fmt"
)

// Two users, different machines, no coordination.
// Same content → same ID. Always.
func Example_noCoordination() {
	alice := New()
	bob := New()

	// Alice and Bob both see the same document
	aliceId := alice.FromString("the quick brown fox")
	bobId := bob.FromString("the quick brown fox")

	fmt.Println(aliceId == bobId)
	// Output: true
}

// A message knows where it came from.
// Change any message in history → every ID after it changes.
func Example_verifiableHistory() {
	engine := New()
	chain := NewChain()

	messages := []string{
		"hello",
		"how are you",
		"goodbye",
	}

	for _, msg := range messages {
		prev := chain.Head()
		id := chain.Next(engine.FromString(msg), engine)
		chain.Advance(id)

		fmt.Printf("prev: %.8s... → msg: %q → id: %.8s...\n", prev, msg, id)
	}
	// Output:
	// prev: 0... → msg: "hello" → id: 338596f9...
	// prev: 338596f9... → msg: "how are you" → id: cb334200...
	// prev: cb334200... → msg: "goodbye" → id: 0dbae342...
}

// Everything connects. Users, posts, comments, likes.
// One web. No foreign keys. No UUIDs. Just content.
func Example_everythingConnects() {
	engine := New()

	// A user
	user := engine.MustFromMap(map[string]any{
		"type":  "user",
		"email": "alice@example.com",
	})

	// A post by that user
	post := engine.Combine(user, engine.MustFromMap(map[string]any{
		"type": "post",
		"body": "Hello world!",
	}))

	// A comment on that post by another user
	bob := engine.MustFromMap(map[string]any{
		"type":  "user",
		"email": "bob@example.com",
	})

	comment := engine.Combine(
		engine.Combine(post, bob),
		engine.FromString("Nice post!"),
	)

	// A like is just: who + what
	like := engine.Combine(bob, post)

	fmt.Printf("user:    %.16s...\n", user)
	fmt.Printf("post:    %.16s...\n", post)
	fmt.Printf("comment: %.16s...\n", comment)
	fmt.Printf("like:    %.16s...\n", like)
	// Output:
	// user:    5811c593f8fc9ae4...
	// post:    60743f64155ad760...
	// comment: c2196cec9351d06e...
	// like:    66c050ac1fd1b42b...
}

// Same action + same history = same result.
// Replay the tape, get the same movie.
func Example_replayable() {
	run := func() ID {
		engine := New()
		chain := NewChain()

		actions := []string{"create", "update", "delete"}
		for _, a := range actions {
			id := chain.Next(engine.FromString(a), engine)
			chain.Advance(id)
		}
		return chain.Head()
	}

	first := run()
	second := run()
	third := run()

	fmt.Println(first == second && second == third)
	// Output: true
}

// Branch, explore, come back.
// Like git, but for any state.
func Example_branch() {
	engine := New()
	chain := NewChain()

	// Build some history
	chain.Advance(chain.Next(engine.FromString("init"), engine))
	chain.Advance(chain.Next(engine.FromString("step1"), engine))

	// Save this point
	checkpoint := chain.Head()

	// Go one direction
	chain.Advance(chain.Next(engine.FromString("path A"), engine))
	pathA := chain.Head()

	// Rewind and go another direction
	chain = NewChainFrom(checkpoint)
	chain.Advance(chain.Next(engine.FromString("path B"), engine))
	pathB := chain.Head()

	fmt.Println(pathA == pathB)
	// Output: false
}

// Order matters. A→B ≠ B→A.
// The sequence is part of the identity.
func Example_orderMatters() {
	engine := New()

	a := engine.FromString("A")
	b := engine.FromString("B")

	// Combine is symmetric (order doesn't matter for combining)
	fmt.Println(engine.Combine(a, b) == engine.Combine(b, a))

	// But chains are not (order matters for sequences)
	chain1 := NewChain()
	chain1.Advance(chain1.Next(a, engine))
	chain1.Advance(chain1.Next(b, engine))

	chain2 := NewChain()
	chain2.Advance(chain2.Next(b, engine))
	chain2.Advance(chain2.Next(a, engine))

	fmt.Println(chain1.Head() == chain2.Head())
	// Output:
	// true
	// false
}
