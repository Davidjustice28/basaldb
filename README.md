# BasalDB

> **The Single-File Matrix-Hierarchical Embedded Database (MHDB)**  
> *Zero-join path traversals, O(1) topological cascade deletions, and Git-style commit accountability.*

---

## What is BasalDB?

**BasalDB** is an open-source, single-file embedded database engine designed specifically for deeply related, strictly hierarchical data (such as multi-tenant SaaS trees, game ECS architectures, and document canvases).

Instead of searching tables via index key lookups or traversing graph pointers, BasalDB maps database depth to physical array row height ($Y$-axis) and entity instances to contiguous offset memory slots ($X$-axis), with attributes sitting on the $Z$-axis.

```
Row 2 (Users)      [ User #0 ]    [ User #1 ]
                       ▲              ▲
                       │ (Children)   │
Row 1 (Teams)      [    Team #101     ]  <-- Children: [User#0, User#1]
                       ▲
                       │ (Children)
Row 0 (Accounts)   [   Account #1     ]  <-- Children: [Team#0, Team#1]
```

---

## Key Features

* **$O(1)$ Matrix Cascade Deletes:** Deleting a parent node instantly invalidates its child array range via top-down memory block pruning—zero recursive table sweeps.
* **Zero-Join Path Traversals:** Navigate hierarchy paths using $O(1)$ array offset math: `Address = BaseAddress + (Y * RowSize) + X`.
* **Git-Style Audit Trail:** Every transaction records an immutable commit block with mandatory `AUTHOR`, `SUMMARY`, and `REASON` metadata.
* **Single-File Portability (`.bsl`):** Zero setup, portable single-file container storage with human-readable ASCII header layers and slotted binary data pages.
* **BasalQL (BQL):** Simple, path-based query language engineered for hierarchical lookups (`GET Accounts[0] -> Teams -> Users`).

---

## File Format Architecture (`.bsl`)

A `.bsl` file is structured into three primary regions:

1. **ASCII Header Block:** Stores version, file signatures, and layer definitions ($Y$-axis heights).
2. **Commit Log Region:** Human-readable append-only audit trail recording every transaction metadata summary.
3. **Slotted Binary Data Pages:** Contiguous fixed-offset memory blocks storing parent pointers, child pointer arrays, and length-prefixed scalar attributes.

---

## When to Use BasalDB

* **Multi-Tenant SaaS Backends:** Isolate customer accounts into separate `.bsl` files with $O(1)$ top-down cascade deletions.
* **Nested Canvas & Design Tools:** Represent canvas layers, groups, and shapes without document re-serialization overhead.
* **Game ECS Systems:** Store game entities and component hierarchies with CPU L1/L2 cache locality.
* **Edge / Local-First Systems:** Run lightweight, high-performance database containers on GCP Cloud Run or embedded edge runtimes.
