# MemQL Service Account Setup

Created: February 9, 2026

## Service Account Details

- **Name**: `memql-deploy`
- **Email**: `memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com`
- **Project**: `fast-fire-486523-f3`
- **Display Name**: MemQL Deploy
- **Description**: MemQL deployment service account

## IAM Roles Granted

The service account has the following permissions:

| Role | Purpose |
|------|---------|
| `roles/cloudbuild.builds.editor` | Build container images with Cloud Build |
| `roles/storage.admin` | Upload/manage build artifacts in Cloud Storage |
| `roles/iam.serviceAccountUser` | Use service accounts for deployments |
| `roles/run.admin` | Deploy and manage Cloud Run services |

## Key File Location

```
/Users/znas/.gcloud/memql-sa-key.json
```

**IMPORTANT**: This file contains sensitive credentials. Never commit it to version control.

## Usage

### Activate the Service Account

```bash
gcloud auth activate-service-account \
  memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com \
  --key-file=/Users/znas/.gcloud/memql-sa-key.json
```

### Verify Activation

```bash
gcloud auth list
```

You should see:

```
ACTIVE  ACCOUNT
*       memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com
```

### Deploy to Cloud Run

Once activated, the service account is automatically used for all `gcloud` commands:

```bash
# Deploy to staging
gcloud run deploy anequim-memql-staging \
  --source . \
  --region us-central1 \
  --platform managed

# Deploy to production
gcloud run deploy anequim-memql-production \
  --source . \
  --region us-west1 \
  --platform managed
```

## Switch Between Accounts

### Switch to MemQL Service Account

```bash
gcloud config set account memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com
```

### Switch to Personal Account

```bash
gcloud config set account google_cloud@visionarys.io
```

### Switch to Frontend Service Account

```bash
gcloud config set account frontend-deploy@fast-fire-486523-f3.iam.gserviceaccount.com
```

## Verification

Test that the service account can access Cloud Run:

```bash
gcloud run services list --region us-central1
```

Expected output:

```
NAME               LABELS                                              URL
anequim-memql-staging  client=anequim,environment=staging,product=memql  https://anequim-memql-staging-...
```

## Integration with Claude Skills

The `/deploy` command should automatically use the active service account when deploying to staging or production environments.

## Security Notes

1. **Key File Security**: The key file is stored with 600 permissions (owner read/write only)
2. **Scope**: This service account only has permissions for Cloud Run deployment and Cloud Build
3. **Separation**: This is a dedicated service account for MemQL, separate from the frontend
4. **Project Isolation**: Service account is scoped to the `fast-fire-486523-f3` project only

## Troubleshooting

### Permission Denied Errors

If you see permission errors during deployment:

```bash
# Re-activate the service account
gcloud auth activate-service-account \
  memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com \
  --key-file=/Users/znas/.gcloud/memql-sa-key.json

# Verify it's active
gcloud auth list
```

### Key File Not Found

Ensure the key file exists:

```bash
ls -la /Users/znas/.gcloud/memql-sa-key.json
```

If missing, recreate it:

```bash
gcloud iam service-accounts keys create /Users/znas/.gcloud/memql-sa-key.json \
  --iam-account=memql-deploy@fast-fire-486523-f3.iam.gserviceaccount.com
```

## Related Documentation

- `/deploy` - Deployment automation skill
- [TECH_STACK_AND_PRACTICES.md](../TECH_STACK_AND_PRACTICES.md) - Deployment practices
- [Google Cloud IAM Best Practices](https://cloud.google.com/iam/docs/best-practices)

---

**Created**: 2026-02-09
**Last Updated**: 2026-02-09
