//! RouterOS REST API client for disk and iSCSI management.

use anyhow::{bail, Context, Result};
use reqwest::Client;
use serde::{Deserialize, Serialize};

/// RouterOS REST API client.
pub struct RouterOsClient {
    client: Client,
    base_url: String,
    user: String,
    password: String,
}

/// A file-backed disk entry from /rest/disk.
#[derive(Debug, Deserialize, Clone)]
pub struct Disk {
    #[serde(rename = ".id")]
    pub id: String,
    #[serde(default)]
    pub slot: String,
    #[serde(rename = "type", default)]
    pub disk_type: String,
    #[serde(rename = "file-path", default)]
    pub file_path: String,
    #[serde(rename = "file-size", default)]
    pub file_size: String,
    #[serde(default)]
    pub fs: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub size: String,
    #[serde(rename = "mount-point", default)]
    pub mount_point: String,
    #[serde(rename = "mount-filesystem", default)]
    pub mount_filesystem: String,
    #[serde(rename = "iscsi-export", default)]
    pub iscsi_export: String,
    #[serde(rename = "iscsi-server-port", default)]
    pub iscsi_server_port: String,
    #[serde(rename = "iscsi-server-iqn", default)]
    pub iscsi_server_iqn: String,
}

impl Disk {
    pub fn is_iscsi_exported(&self) -> bool {
        self.iscsi_export == "true" || self.iscsi_export == "yes"
    }

    pub fn is_file_backed(&self) -> bool {
        self.disk_type == "file"
    }
}

/// Parameters for creating a file-backed disk.
#[derive(Debug, Serialize)]
pub struct CreateFileDiskParams {
    #[serde(rename = "type")]
    pub disk_type: String,
    #[serde(rename = "file-path")]
    pub file_path: String,
    #[serde(rename = "file-size", skip_serializing_if = "Option::is_none")]
    pub file_size: Option<String>,
}

impl RouterOsClient {
    /// Create a new client. base_url should be like "http://192.168.200.1".
    pub fn new(base_url: &str, user: &str, password: &str) -> Result<Self> {
        let client = Client::builder()
            .danger_accept_invalid_certs(true)
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .context("building HTTP client")?;

        Ok(Self {
            client,
            base_url: base_url.trim_end_matches('/').to_string(),
            user: user.to_string(),
            password: password.to_string(),
        })
    }

    /// GET a REST endpoint and parse JSON response.
    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T> {
        let url = format!("{}/rest{}", self.base_url, path);
        let resp = self
            .client
            .get(&url)
            .basic_auth(&self.user, Some(&self.password))
            .send()
            .await
            .with_context(|| format!("GET {url}"))?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            bail!("GET {url} returned {status}: {body}");
        }

        resp.json().await.with_context(|| format!("parsing response from {url}"))
    }

    /// POST to a REST endpoint with a JSON body.
    async fn post<B: Serialize>(&self, path: &str, body: &B) -> Result<serde_json::Value> {
        let url = format!("{}/rest{}", self.base_url, path);
        let resp = self
            .client
            .post(&url)
            .basic_auth(&self.user, Some(&self.password))
            .json(body)
            .send()
            .await
            .with_context(|| format!("POST {url}"))?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body_text = resp.text().await.unwrap_or_default();
            bail!("POST {url} returned {status}: {body_text}");
        }

        let text = resp.text().await.unwrap_or_default();
        if text.is_empty() {
            Ok(serde_json::Value::Null)
        } else {
            serde_json::from_str(&text).with_context(|| format!("parsing response from {url}"))
        }
    }

    /// List all disks.
    pub async fn list_disks(&self) -> Result<Vec<Disk>> {
        self.get("/disk").await
    }

    /// List only file-backed disks.
    pub async fn list_file_disks(&self) -> Result<Vec<Disk>> {
        let disks: Vec<Disk> = self.get("/disk").await?;
        Ok(disks.into_iter().filter(|d| d.is_file_backed()).collect())
    }

    /// Find a disk by file-path.
    pub async fn find_disk_by_path(&self, file_path: &str) -> Result<Option<Disk>> {
        let disks = self.list_file_disks().await?;
        let normalized = file_path.trim_start_matches('/');
        Ok(disks.into_iter().find(|d| {
            d.file_path.trim_start_matches('/') == normalized
        }))
    }

    /// Create a file-backed disk and return its RouterOS .id.
    pub async fn create_file_disk(&self, file_path: &str, file_size: Option<&str>) -> Result<String> {
        let params = CreateFileDiskParams {
            disk_type: "file".to_string(),
            file_path: format!("/{}", file_path.trim_start_matches('/')),
            file_size: file_size.map(|s| s.to_string()),
        };

        tracing::info!("creating file disk: {}", params.file_path);
        self.post("/disk/add", &params).await?;

        // Find the newly created disk by path
        let disk = self
            .find_disk_by_path(file_path)
            .await?
            .with_context(|| format!("created file disk but cannot find it: {file_path}"))?;

        tracing::info!("file disk created: id={} slot={}", disk.id, disk.slot);
        Ok(disk.id)
    }

    /// Enable iSCSI export on a disk.
    pub async fn enable_iscsi_export(&self, disk_id: &str) -> Result<()> {
        tracing::info!("enabling iSCSI export on disk {disk_id}");
        self.post(
            "/disk/set",
            &serde_json::json!({
                ".id": disk_id,
                "iscsi-export": "yes",
            }),
        )
        .await?;
        Ok(())
    }

    /// Disable iSCSI export on a disk.
    pub async fn disable_iscsi_export(&self, disk_id: &str) -> Result<()> {
        tracing::info!("disabling iSCSI export on disk {disk_id}");
        self.post(
            "/disk/set",
            &serde_json::json!({
                ".id": disk_id,
                "iscsi-export": "no",
            }),
        )
        .await?;
        Ok(())
    }

    /// Remove a disk entry from RouterOS.
    pub async fn remove_disk(&self, disk_id: &str) -> Result<()> {
        tracing::info!("removing disk {disk_id}");
        self.post(
            "/disk/remove",
            &serde_json::json!({ ".id": disk_id }),
        )
        .await?;
        Ok(())
    }

    /// Create a file-backed disk and export it via iSCSI in one step.
    /// Returns (disk_id, iqn, portal_port).
    pub async fn create_iscsi_target(
        &self,
        file_path: &str,
        file_size: Option<&str>,
    ) -> Result<(String, String, u16)> {
        let disk_id = self.create_file_disk(file_path, file_size).await?;
        self.enable_iscsi_export(&disk_id).await?;

        // Re-fetch to get the IQN
        let disk = self
            .find_disk_by_path(file_path)
            .await?
            .with_context(|| "disk disappeared after iSCSI enable")?;

        let port = disk
            .iscsi_server_port
            .parse::<u16>()
            .unwrap_or(3260);

        tracing::info!(
            "iSCSI target ready: iqn={} port={}",
            disk.iscsi_server_iqn,
            port
        );

        Ok((disk_id, disk.iscsi_server_iqn, port))
    }

    /// Delete an iSCSI target: disable export, remove disk entry.
    pub async fn delete_iscsi_target(&self, disk_id: &str) -> Result<()> {
        self.disable_iscsi_export(disk_id).await?;
        self.remove_disk(disk_id).await?;
        Ok(())
    }

    /// Format a disk via RouterOS (set format-drive property).
    pub async fn format_disk(&self, disk_id: &str, filesystem: &str) -> Result<()> {
        tracing::info!("formatting disk {disk_id} as {filesystem}");
        self.post(
            "/disk/format-drive",
            &serde_json::json!({
                ".id": disk_id,
                "file-system": filesystem,
            }),
        )
        .await?;
        Ok(())
    }

    /// Mount a formatted disk.
    pub async fn mount_disk(&self, disk_id: &str) -> Result<()> {
        tracing::info!("mounting disk {disk_id}");
        self.post(
            "/disk/set",
            &serde_json::json!({
                ".id": disk_id,
                "mount-filesystem": "yes",
            }),
        )
        .await?;
        Ok(())
    }

    /// Create a sparse file via RouterOS file system.
    /// Uses /file/add which creates a file on the RouterOS filesystem.
    pub async fn create_file(&self, path: &str, contents: &[u8]) -> Result<()> {
        let ros_path = format!("/{}", path.trim_start_matches('/'));
        tracing::info!("creating file: {ros_path}");
        self.post(
            "/file/add",
            &serde_json::json!({
                "name": ros_path,
                "contents": String::from_utf8_lossy(contents),
            }),
        )
        .await?;
        Ok(())
    }

    /// Get system resource info (useful for testing connectivity).
    pub async fn system_resource(&self) -> Result<serde_json::Value> {
        self.get("/system/resource").await
    }
}
