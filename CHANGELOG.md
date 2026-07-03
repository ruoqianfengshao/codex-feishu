# Changelog

## Unreleased

## v0.6.10

- Added Feishu OpenAPI auth recovery: when a send fails because the access token is invalid or expired, the bot rebuilds the API client, refreshes the tenant access token, and retries the original send once.
- Prevented Feishu card fallback from hiding auth recovery failures.

## v0.6.9

- Added Feishu workflow screenshots to the English and Chinese README files.
- Used a three-column screenshot layout for GitHub README rendering.

## v0.6.8 and earlier

- Removed legacy chat adapter support and old observer-mode product docs.
- Kept the product focused on Feishu/Lark bot DM workspace and Codex thread topics.
- Renamed shared message formatting code away from legacy adapter-specific naming.
- Removed old global observer and run notice storage state.
