package accountexport

// resetHandOversForTest empties the registry and answers a function that
// puts back what was there.
func resetHandOversForTest() func() {
	handOvers.Lock()
	saved := handOvers.m
	handOvers.m = map[string]HandOver{}
	handOvers.Unlock()
	return func() {
		handOvers.Lock()
		handOvers.m = saved
		handOvers.Unlock()
	}
}
