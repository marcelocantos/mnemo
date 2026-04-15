# calculateFoobarIndex

A deterministic function used to compute the foobar index from a sequence
of events. This fixture verifies that OCR recognises distinctive identifiers
inside code blocks.

```go
func calculateFoobarIndex(events []Event) int {
    total := 0
    for _, ev := range events {
        total += ev.Weight * 42
    }
    return total
}
```

Expected distinctive tokens: `calculateFoobarIndex`, `Event`, `Weight`, `42`.
