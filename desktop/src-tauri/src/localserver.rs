// 在本机运行服务器：把随 app 打包的 hearth 服务端（Tauri externalBin）装成用户级服务、
// 拉起来，网页随后当成一台普通的 hearth 连（https://127.0.0.1:8080）。
//
// 四条边界：
//   - 常驻与状态全部交给服务端自己的 `service` CLI（install/start/stop/status --json），
//     壳这边不另做一套守护逻辑，也不解析人读文案。
//   - 数据目录固定在 app 数据目录下的 server/，与桌面端其它数据分开；卸载 app 即带走。
//   - 首个账号走 `adduser`（空库首个账号自动 super），账号密码只作为进程参数透传，不进日志。
//   - 本机自签根证书的指纹从**本机文件**算出来，再走既有的配对流程落锚：与远端服务器
//     一样只给这一台加信任锚，不装系统 CA。
use std::fs;
use std::io::{Read, Seek, SeekFrom};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Arc;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use tauri::Manager;

use crate::trust::{self, Trust};

/// 启动后等服务端应答 /healthz 的上限。
const READY_TIMEOUT: Duration = Duration::from_secs(20);

/// 监听端口：服务端 ADDR 的默认值就是 8080（合并模式下 http 与 https 同端口）。
/// 端口被占用不自动换——换了网页与 OBS 的地址就都对不上，错误原样带回界面由用户处置。
/// HEARTH_DESKTOP_LOCAL_PORT / HEARTH_DESKTOP_DATA_DIR 仅供开发期调试（端到端验证时
/// 不污染真实数据目录、绕开被占用的端口），正常运行不该设。
fn port() -> u16 {
    std::env::var("HEARTH_DESKTOP_LOCAL_PORT")
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(8080)
}

fn base_url() -> String {
    format!("https://127.0.0.1:{}", port())
}

/// sidecar 可执行文件：Tauri 把 externalBin 放在主程序旁边（macOS 是 Contents/MacOS/），
/// 文件名去掉 host triple 后缀。没打进去（如当前的 Windows 包）就是没有这项能力。
pub fn sidecar() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    let path = exe.parent()?.join("hearth");
    path.is_file().then_some(path)
}

fn data_dir(app: &tauri::AppHandle) -> Result<PathBuf, String> {
    if let Ok(v) = std::env::var("HEARTH_DESKTOP_DATA_DIR") {
        if !v.trim().is_empty() {
            return Ok(PathBuf::from(v));
        }
    }
    app.path()
        .app_data_dir()
        .map(|d| d.join("server"))
        .map_err(|e| format!("取数据目录失败：{e}"))
}

/// 跑一次 sidecar 子命令。label 是出错时给用户看的名字——args 里可能有密码，不能进文案。
fn run(exe: &Path, data: &Path, args: &[&str], label: &str) -> Result<String, String> {
    let out = Command::new(exe)
        .arg("--data")
        .arg(data)
        .args(args)
        .output()
        .map_err(|e| format!("{label}失败：{e}"))?;
    let stdout = String::from_utf8_lossy(&out.stdout).into_owned();
    if !out.status.success() {
        let mut msg = String::from_utf8_lossy(&out.stderr).trim().to_string();
        if msg.is_empty() {
            msg = stdout.trim().to_string();
        }
        if msg.is_empty() {
            msg = format!("退出码 {}", out.status.code().unwrap_or(-1));
        }
        return Err(format!("{label}失败：{msg}"));
    }
    Ok(stdout)
}

/// `service status --json` 的解析结果；多出来的字段（detail/pid）只给人看，这里不要。
#[derive(Deserialize)]
struct ServiceState {
    installed: bool,
    running: bool,
}

fn service_state(exe: &Path, data: &Path) -> Result<ServiceState, String> {
    let out = run(exe, data, &["service", "status", "--json"], "查询本机服务状态")?;
    serde_json::from_str(out.trim()).map_err(|e| format!("服务状态解析失败：{e}"))
}

#[derive(Serialize)]
pub struct LocalServerStatus {
    /// 这个安装包有没有带服务端程序
    available: bool,
    installed: bool,
    running: bool,
    url: String,
}

#[tauri::command(async)]
pub async fn local_server_status(app: tauri::AppHandle) -> Result<LocalServerStatus, String> {
    let Some(exe) = sidecar() else {
        return Ok(LocalServerStatus {
            available: false,
            installed: false,
            running: false,
            url: base_url(),
        });
    };
    let data = data_dir(&app)?;
    let st = service_state(&exe, &data)?;
    Ok(LocalServerStatus {
        available: true,
        installed: st.installed,
        running: st.running,
        url: base_url(),
    })
}

#[derive(Serialize)]
pub struct LocalServerStarted {
    url: String,
    /// 本次是否新建了管理员账号：true 时网页可以直接拿这对用户名密码登录。
    /// 账号早已存在（重装、换过端口等）按 false，让用户走正常登录。
    initialized: bool,
}

#[tauri::command(async, rename_all = "snake_case")]
pub async fn local_server_start(
    app: tauri::AppHandle,
    trust: tauri::State<'_, Arc<Trust>>,
    username: Option<String>,
    password: Option<String>,
) -> Result<LocalServerStarted, String> {
    let exe = sidecar().ok_or("这个安装包没有带服务端程序")?;
    let data = data_dir(&app)?;
    let creds = match (username, password) {
        (Some(u), Some(p)) if !u.is_empty() || !p.is_empty() => Some(check_creds(u, p)?),
        _ => None,
    };

    let st = service_state(&exe, &data)?;

    // 建账号放在拉起服务之前：此时没有第二个进程开着同一个 sqlite 文件。
    // 用户已存在是「早就初始化过」，不是失败。
    let mut initialized = false;
    if let Some((u, p)) = &creds {
        match run(&exe, &data, &["adduser", u, p], "创建管理员账号") {
            Ok(_) => initialized = true,
            Err(e) if user_exists(&e) => {}
            Err(e) => return Err(e),
        }
    }

    // install 自带装载并拉起，装完不必再 start（再 start 会立刻重启一次）。
    if !st.installed {
        run(&exe, &data, &["service", "install"], "安装本机服务")?;
    } else if !st.running {
        run(&exe, &data, &["service", "start"], "启动本机服务")?;
    }

    wait_ready(&data).await?;
    pair_local(&trust, &data).await?;
    Ok(LocalServerStarted {
        url: base_url(),
        initialized,
    })
}

#[tauri::command(async)]
pub async fn local_server_stop(app: tauri::AppHandle) -> Result<(), String> {
    let exe = sidecar().ok_or("这个安装包没有带服务端程序")?;
    let data = data_dir(&app)?;
    run(&exe, &data, &["service", "stop"], "停止本机服务")?;
    Ok(())
}

/// 账号密码先在壳里卡一道（与服务端 adduser 同一套规则），不合法的值不进程序参数。
fn check_creds(username: String, password: String) -> Result<(String, String), String> {
    let u = username.trim().to_string();
    if u.len() < 2
        || u.len() > 32
        || !u.chars().all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
    {
        return Err("用户名需 2-32 位字母、数字、下划线或连字符".into());
    }
    if password.len() < 6 || password.len() > 128 {
        return Err("密码至少 6 位".into());
    }
    Ok((u, password))
}

/// adduser 撞上已有用户名时 sqlite 报唯一约束；文案随方言变，认关键片段即可。
fn user_exists(err: &str) -> bool {
    let e = err.to_ascii_lowercase();
    e.contains("unique") || e.contains("duplicate") || e.contains("已存在")
}

/// 等服务端起来：/healthz 只表示进程活着，正是这里要的。超时把日志尾巴带回去——
/// 端口被占用之类的失败只会写进日志，不带出来界面上就只剩一句「超时」。
async fn wait_ready(data: &Path) -> Result<(), String> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(2))
        .build()
        .map_err(|e| format!("创建 http 客户端失败：{e}"))?;
    let url = format!("http://127.0.0.1:{}/healthz", port());
    let deadline = Instant::now() + READY_TIMEOUT;
    loop {
        if let Ok(resp) = client.get(&url).send().await {
            if resp.status().is_success() {
                return Ok(());
            }
        }
        if Instant::now() >= deadline {
            let tail = log_tail(data);
            return Err(if tail.is_empty() {
                "服务端启动超时".to_string()
            } else {
                format!("服务端启动超时：{tail}")
            });
        }
        tokio::time::sleep(Duration::from_millis(400)).await;
    }
}

/// 日志尾巴：服务模式的日志落 <data>/hearth.log，launchd 自己抓的在 hearth-launchd.log。
fn log_tail(data: &Path) -> String {
    for name in ["hearth.log", "hearth-launchd.log"] {
        let Ok(mut f) = fs::File::open(data.join(name)) else {
            continue;
        };
        let Ok(meta) = f.metadata() else { continue };
        if f.seek(SeekFrom::Start(meta.len().saturating_sub(4096))).is_err() {
            continue;
        }
        let mut buf = Vec::new();
        if f.read_to_end(&mut buf).is_err() {
            continue;
        }
        let text = String::from_utf8_lossy(&buf);
        let mut lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
        // 首行可能被 4096 的边界切断，丢掉
        if meta.len() > 4096 && lines.len() > 1 {
            lines.remove(0);
        }
        let tail: Vec<&str> = lines.into_iter().rev().take(3).collect();
        if !tail.is_empty() {
            return tail.into_iter().rev().collect::<Vec<_>>().join("；");
        }
    }
    String::new()
}

/// 给本机服务器落信任锚：根证书就在本机磁盘上，指纹从文件算，再走与远端服务器同一条
/// 配对路径（下载 /ca.crt、比指纹、真实校验一次）。已经信任过就什么都不做。
async fn pair_local(trust: &Trust, data: &Path) -> Result<(), String> {
    let url = base_url();
    if trust::check(trust, &url).await?.ok {
        return Ok(());
    }
    let ca = wait_ca_cert(data).await?;
    let der = trust::first_cert_der(&ca)?;
    let fingerprint = Sha256::digest(&der)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect::<String>();
    trust::pair(trust, &url, &fingerprint).await
}

/// 自签根证书在服务端起来后的第一轮检查里生成，可能比 /healthz 晚一点点。
async fn wait_ca_cert(data: &Path) -> Result<Vec<u8>, String> {
    let path = data.join("tls").join("ca.crt");
    for _ in 0..25 {
        if let Ok(b) = fs::read(&path) {
            if !b.is_empty() {
                return Ok(b);
            }
        }
        tokio::time::sleep(Duration::from_millis(400)).await;
    }
    Err("本机服务器没有生成自签根证书（TLS 证书来源被改成了别的？）".to_string())
}
