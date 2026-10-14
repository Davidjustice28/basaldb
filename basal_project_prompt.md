# BasalDB Master Implementation Prompt

Use this document as the prompt for Cursor, Windsurf, or LLM code generation to implement BasalDB in Go.

## Architecture
- **Type:** Matrix-Hierarchical Database (MHDB)
- **Addressing:** `<Y, X, Z>` where Y = Layer Height, X = Sequence Slot, Z = Attribute
- **Format:** Single `.bsl` file with ASCII Header, Symbol-delimited Commit Log, and Binary Slotted Pages.

## Goals
1. Implement BQL parser in Go (`pkg/bql`).
2. Implement binary slotted page engine (`pkg/engine`).
3. Build REST API daemon `basald` (`cmd/basald`).
4. Containerize with Docker and prepare Cloud Run deployment scripts.
