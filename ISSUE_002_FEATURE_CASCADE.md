# Issue #2: Implement O(1) Matrix Cascade Invalidation Engine

## Description
Build the bitmask memory block invalidation logic in `pkg/engine/cascade.go`.

## Requirements
1. Read target record's child pointer array.
2. Perform top-down recursive bitmask flipping in slotted binary pages.
3. Write an immutable commit log entry with mandatory `Author`, `Summary`, and `Reason` fields.
4. Ensure execution time remains constant $O(1)$ regardless of tree size.
