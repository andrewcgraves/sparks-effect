## Summary

## How it was checked

## Checklist

- [ ] `make prepush` passed.
- [ ] Contract fixtures changed on both sides, in `api/` and `worker/`. N/A if no fixture was touched.
- [ ] `website/src/api/` updated to match. N/A if no API response changed shape.
- [ ] No migration drops or renames anything the live API still reads. N/A if no migrations.
- [ ] Nothing copied from `kustomize-config`.
