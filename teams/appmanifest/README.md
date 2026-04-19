# Teams App Manifest

This directory contains the Teams app manifest and icons required for publishing to Microsoft Teams.

## Files

| File | Description |
|------|-------------|
| `manifest.json` | App manifest — replace `{{MICROSOFT_APP_ID}}` with your Azure AD App Registration ID |
| `color.png` | **192x192 px** full-color icon (PNG) — you must create this |
| `outline.png` | **32x32 px** white outline on transparent background (PNG) — you must create this |

## Setup

1. Replace `{{MICROSOFT_APP_ID}}` in `manifest.json` with your Azure Bot / App Registration ID
2. Create `color.png` (192x192) and `outline.png` (32x32 white+transparent)
3. Zip all three files into a `.zip` package:

```bash
cd teams/appmanifest
zip quill-teams.zip manifest.json color.png outline.png
```

4. Upload to [Teams Developer Portal](https://dev.teams.microsoft.com/apps) > Apps > Import app

## Azure Bot Registration

Before the manifest works, you need an Azure Bot resource:

1. Go to [Azure Portal](https://portal.azure.com) > Create "Azure Bot"
2. Note the **App ID** and **App Secret** (client secret)
3. Set the messaging endpoint to: `https://<your-domain>/api/messages`
4. Add these to your `config.toml`:

```toml
[teams]
app_id = "<App ID from Azure>"
app_secret = "<App Secret from Azure>"
tenant_id = "<Your Tenant ID>"
listen = ":3978"
```
