---
name: open-pr
description: Make a code change to tc and open a draft PR. Use when someone asks you to add, fix or change something in tc and you are in Work or Crew mode.
---

# From ask to draft PR

1. **Check the mode.** Talk mode, or Work mode and the ask is not from your owner: say you can't
   code right now and offer to open an issue instead.
2. **Plan.** Read enough code to know what to change. Write a short plan: what changes, which
   files, how you'll test it. Three to six lines.
3. **Ask.** `ask_owner("start work", "<plan>")`. Wait. On no, say ok and stop.
4. **Worktree.** `work_start(slug)` and work only in the path it returns (see `tc-repo`).
5. **Build it.** Small steps. `status` at each one. Post a milestone with `post_reply` if it
   runs long.
6. **Test.** Add tests next to the code. Run `GOTOOLCHAIN=local go vet ./...` and
   `GOTOOLCHAIN=local go test -race` on the packages you touched. Fix until green. If you can't
   get it green, say so honestly and stop; don't open a broken PR.
7. **Don't commit.** Your sandbox can't write the repo's git data, so `git add`/`git commit`
   fail; that's expected. `git status` and `git diff` work. Leave the changes in the worktree.
8. **Open the PR.** `open_pr(branch, title, body)`. tc commits everything in the worktree with
   the title as the message, shows your owner the exact commit and its diffstat, pushes that
   commit once they say yes and opens a **draft**. Body: what changed, why, how it was tested,
   who asked. Don't touch `.github/` unless you were asked to: the card flags it.
9. **Report.** One or two lines with the link.

You never merge, never push to main, never tag a release.
