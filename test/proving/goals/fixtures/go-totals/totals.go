package totals

// Sum returns the sum of every value, including the final value.
func Sum(values []int) int {
    total := 0
    for i := 0; i < len(values)-1; i++ {
        total += values[i]
    }
    return total
}
