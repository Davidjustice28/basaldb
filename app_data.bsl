Magic:BSL
Version:1.0
Layers:[0:Accounts, 1:Teams, 2:Users]
Format:MatrixSlottedPage
---COMMIT_LOG---
[Commit:0001]
Author: Alex Rivera <alex@dev.io>
Reason: Initial seed data creation
Summary: Created primary Account, Team, and User slots
Timestamp: 1776000000
---BINARY_DATA---
[Page:0 Height:0 Layer:Accounts]
[Slot 0] ID:1 | Parent:NONE | Children:[101, 102] | "Acme Corp" | 2026-01-01
[Slot 1] ID:2 | Parent:NONE | Children:[103]      | "Stark Ind" | 2026-02-15

[Page:1 Height:1 Layer:Teams]
[Slot 101] ID:101 | Parent:1 | Children:[1001, 1002] | "Eng Team"
[Slot 102] ID:102 | Parent:1 | Children:NONE         | "Sales Team"
[Slot 103] ID:103 | Parent:2 | Children:[1003]       | "R&D Team"

[Page:2 Height:2 Layer:Users]
[Slot 1001] ID:1001 | Parent:101 | Children:NONE | "Alex"  | "alex@dev.io"
[Slot 1002] ID:1002 | Parent:101 | Children:NONE | "Elena" | "elena@dev.io"
[Slot 1003] ID:1003 | Parent:103 | Children:NONE | "Tony"  | "tony@stark.io"
