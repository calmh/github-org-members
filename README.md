# github-org-members

This utility lists current organisation members and recent commit activity
for all public repositories and makes a recommendation about membership
changes. The specific values are configurable, but by default it recommends
inviting contributors who have authored five commits in the last year, and
removing members who have not authored any commits in the last five years.

Needs `GITHUB_TOKEN` and `GITHUB_ORGANISATION` in the environment.

```
on:
  workflow_dispatch:
  schedule:
    - cron: '0 0 1 * *'

jobs:

  run-recommendation:
    runs-on: ubuntu-latest
    name: Check for a recommendation
    steps:

      - uses: docker://ghcr.io/calmh/github-org-members:latest
        env:
          GITHUB_TOKEN: ...
          GITHUB_ORGANISATION: ...
          GOM_IGNORE_USERS: ...
```
