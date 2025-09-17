# Scan infected npm packages in actions

This tool fetches all github actions used in an organisation's repository, checks if it uses npm and if so, checks if any of the npm packages or its dependencies are considered infected ([Shai-Hulud attack](https://krebsonsecurity.com/2025/09/self-replicating-worm-hits-180-software-packages/)).

## Usage

This assumes you have [Go installed](https://go.dev/doc/install).

1. Create a Github classic token with repo scope and permissions in the Github organisation.
2. `export GITHUB_TOKEN=[token]`
3. `export GITHUB_ORG=[github org name]`
4. `go run .`
