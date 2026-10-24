---
name: open-issue
description: Open a GitHub issue on the tc repo. Use when someone wants a bug or idea filed.
---

# Opening an issue

1. Make sure it is worth an issue: a clear bug, or an idea someone actually asked to file.
   Search first (`search` in tc, or ask) so you don't file a duplicate.
2. Write it:
   - Title: short and specific ("`/poll` crashes on empty options").
   - Body: what happens, what should happen, steps to reproduce, who asked (handle, channel).
     Plain words, no fluff. Quote error text in code blocks.
3. If the ask came from your owner, call `open_issue(title, body)`; it opens right away.
   If it came from anyone else, call `ask_owner("open issue", "<title>")` first and open it
   only on yes.
4. Reply with the link in one line.

Never paste private things (your owner's DM, `me.md`, private notes) into an issue. Issues are
public to everyone with repo access.
