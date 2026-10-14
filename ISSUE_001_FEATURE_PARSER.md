# Issue #1: Implement .bsl Header & Layer Definition Parser

## Description
Build the core Go package (`pkg/engine/header.go`) to read and parse the ASCII header block and layer definitions of a `.bsl` file.

## Requirements
1. Verify magic signature `Magic:BSL`.
2. Parse layer definitions into `map[int]Layer` where key is layer height ($Y$-axis).
3. Extract commit log offset boundaries.
4. Provide unit tests covering valid headers and corrupted files.
