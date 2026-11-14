# Todo

Every open task for goodreads-nextread, and only here. Most important first. When an item is
done, delete it and put what's now true into its home ([architecture.md](architecture.md),
[inventory.md](inventory.md), [decisions.md](decisions.md)).

## Open

- [ ] **Verify whether a shelf entry keeps its "date added" when it moves between shelves**
  (added 2026-11-14) — it keeps its review id (observed with an entry that moved to
  `currently-reading`). If it also keeps the original date, an entry moved back to the shelf
  sorts outside the quick check's newest 100, which is one of the two reasons for the
  safety-net harvest ([architecture.md](architecture.md#safety-net-harvest-and-the-quick-checks-blind-spots)).
  Check: on a test account, add a book to `to-read`, move it to another shelf and back a day
  later, and request the widget with `sort=date_added&order=d`. The book first in the list
  means the date is reset. Then update the architecture section and the comment on
  `DefaultFullEvery` in `internal/shelfsync/sync.go`.
