# IDM (Internet Download Manager 6.43) — 逆向证据账本

目标产物：`C:\Program Files (x86)\Internet Download Manager\IDMan.exe`
SHA-256：`cd14784a3b8ad439e423083229709cc0881c0b794a8ab6f49c357639336bd1f5`
大小：6,734,704 字节

证据来源分三层，下文用 `[obs]`（静态观测）/ `[doc]`（厂商随包文档）/ `[inf]`（推断）标注。

| 编号 | 操作 | 证据 ID / 文件 | 权限 |
|---|---|---|---|
| E1 | `rea inspect-artifact` | `ev_04e95482db269cfb6b0443e8a1271cdbc0f7471d732d4743d78ae5dc37d844b7` | shipped-artifact |
| E2 | 自写 PE 解析（`tools/pe.js`） | 本文件 §1 | shipped-artifact |
| E3 | 字符串抽取（`tools/strings.js` → `re/idman.ascii.txt` / `re/idman.utf16.txt`） | 本文件 §2–§6 | shipped-artifact |
| E4 | 随包浏览器扩展解包（`tools/unzip.js` → `re/ext_chrome`, `re/ext_edge`, `re/ext_ff*`） | 本文件 §7 | shipped-artifact |
| E5 | 随包 CHM 解编译（`hh.exe -decompile` → `re/chm_idman`, `re/chm_sched`, `re/chm_grab`） | 本文件 §8 | shipped-artifact |

分析引擎状态：Ghidra 未配置（`GHIDRA_INSTALL_DIR` 未设置，且本机 JDK 17 低于 Ghidra 12.1 要求的 21+），IDA MCP 未配置，Hopper 不支持 win32。因此**没有函数级反编译证据**，所有结论止于产物级观测（PE 结构、字符串、随包资源、随包文档）。这一点在 §9 未知项中保留。

---

## 1. PE 结构 [obs, E2]

```
machine        = 0x14c (x86)
magic          = PE32
subsystem      = 2 (GUI)
dllChars       = 0x8140  FORCE_INTEGRITY NX_COMPAT TERMINAL_SERVER_AWARE
imageBase      = 0x400000
entryPoint RVA = 0x2b07bc
sections       = .text (2.9 MB, XR) .rdata (1.0 MB, R) .data (0.4 MB, RW) .rsrc (2.2 MB, R) .reloc
```

导入表只有 `KERNEL32 / USER32 / GDI32 / COMDLG32 / ADVAPI32 / COMCTL32 / SHLWAPI`，另有 delay-import 目录（RVA 0x3f3258，256 字节）。KERNEL32 导入形态（`LockFile`、`GetProfileIntA`、`GlobalHandle`、`lstrcpyW`、`TlsAlloc`…）是 Delphi/Embarcadero RTL 的典型签名 → 主程序为 Delphi 编写。

**关键含义**：WinSock / WinINet / WinHTTP 都不在静态导入表里，网络层是运行期动态解析的。这直接解释为什么 `[obs]` 字符串里出现的是**手写的完整 HTTP/1.1 报文模板**（§3），而不是 `HttpSendRequest` 之类的 API 调用。

## 2. 网络请求模板 [obs, E3 @ idman.ascii.txt:10955–11032]

```
%s %s HTTP/1.1                     ← "GET /path HTTP/1.1"
Host: %s%s
User-Agent: %s
Accept: %s
Connection: Keep-Alive
Cache-Control: no-cache
Pragma: no-cache
Accept-Encoding: identity          ← 分段请求必须 identity，否则字节偏移错位
Accept-Encoding: gzip, deflate     ← 探测/整文件请求
Accept-Charset: * / Accept-Charset: %s
Origin: %s
Accept-Language: %s
Cookie2: %s
Content-Type: %s
Content-Length: %d
If-Modified-Since: %s
Referer: %s

Range: bytes=0-%I64d
Range: bytes=0-
Range: bytes=%I64d-
Range: bytes=%I64d-%I64d

Authorization: Digest username="%s", realm="%s", nonce="%s", uri="%s", %sresponse="%s"%s
  , opaque="%s" / qop=%s, nc=%s, cnonce="%s",
Proxy-Authorization: Basic %s
Proxy-Authorization: Digest username="%s", realm="%s", nonce="%s", uri="%s", %sresponse="%s"%s%s
CONNECT %s:%ld HTTP/1.1
Host: %s:%ld
```

响应侧解析的字段 [obs]：`content-length:`、`Content-Range:`、`multipart/byteranges`、`accept-ranges:`、`Content-Encoding:`（含 `utf-8`、`windows-125`、`ISO-8859`、RFC2047 `=?UTF8?B?`）、`Content-Disposition:`（`filename`/`attachment`/`inline`）、`Last-Modified:`、`Set-Cookie:`、`retry-after`、`WWW-Authenticate:`（`Digest`/`Bearer`/`realm`/`nonce`/`stale`/`opaque`/`algorithm`）。

## 3. 文件类型识别 [obs]

`Content-Type` 分类里出现 `audio/`、`video/`、`video/MP2T`、`application/vnd.apple.mpegurl`、`application/x-mpegURL`，另有 `flv` 结构与 `video/*` 嗅探报错（`Checking flv file struct.`、`Incomplete fragment`）。协议前缀集合：`http:// https:// ftp:// mms:// rtmp:// rtmpt:// test://`。

## 4. 配置项（注册表值名）[obs, E3 @ 7236–7260, 9881, 9914–9916]

```
MaxConnectionsNumber
ProxyEnable  ProxyServer
UseHttpProxy  HttpProxy  HttpPort  HttpUserName  HttpPasword  HttpEncPassword
UseHttpsProxy HttpsProxy HttpsPort HttpsUserName HttpsPasword HttpsEncPassword
UseFtpProxy   FtpProxy   FtpPort   FtpUserName   FtpPasword   FtpEncPassword  FtpPasive
bUseSocks     SocksType  SocksHost SocksPort     SocksUser    SocksPass      Socks5ProxyDNS
AdvIntDriverEnabled2
```

注册表根 [obs]：`SOFTWARE\Internet Download Manager`、`Software\DownloadManager\IDMBI`、`Software\DownloadManager\GrabberSts`、`Software\Internet Download Manager`（另有 `...\IDMBI\IEXPLORE\DwnlPanel`）。注意 `Pasword`（单 s）是厂商拼写，复刻时保持原样以便读同一份配置。

## 5. 临时文件与续传 [obs, E3]

```
%s.tmp            (idman.ascii.txt:10342)   ← 下载中临时文件
%s.result.tmp
%s.mp4.tmp
%s.cnv.tmp
Deleting temporary files
The file size has been changed on the server.
When trying to resume the download, Internet Download Manager got a response from the server that it doesn't support resuming the download.
The data transfer has been interrupted and the server does not support "resume".
```

## 6. 失败处理与降级 [obs, E3]

```
Connection restarted because of timeout (1)/(2)/(3)
Connection timed out, restarting...
IDM started to download the file using one connection because of data corruption
  when newly received data in several connections did not match the same data downloaded earlier.
Cannot connect to the server
Cannot connect to proxy server %s:%ld
Cannot connect to %s:%ld
Cannot connect to the socks server %s
IDM has received "Access Denied" error while assembling file parts.
```

→ 观测到三类降级：超时重连、数据不一致时**降到单连接**、分片合并失败可重试。

## 7. 浏览器集成 [obs, E4]

`re/ext_chrome/manifest.json`：
- MV3，`background.service_worker: background.js`，版本 `6.43.18`，author `Tonec FZE`
- 权限：`scripting tabs cookies contextMenus webNavigation webRequest declarativeNetRequest downloads downloads.shelf downloads.ui management storage proxy nativeMessaging`
- `content_scripts` 覆盖 `*://*/*`、`all_frames: true`、`run_at: document_start`

`background.js` 两条传输通道 [obs]：
- WebSocket：`ws://<127.0.0.1>/ ?cid=…&rnd=…`，子协议 `plugin.v3.internetdownloadmanager.com`
- Native Messaging：`browser.runtime.connectNative("com.tonec.idm")`

Native host 注册位置 [obs, E3 @ 7382–7476]：
```
SOFTWARE\Google\Chrome\NativeMessagingHosts\com.tonec.idm
SOFTWARE\Mozilla\NativeMessagingHosts\com.tonec.idm
SOFTWARE\Microsoft\Edge\NativeMessagingHosts\com.tonec.idm
SOFTWARE\CocCoc\Browser\NativeMessagingHosts\com.tonec.idm
```

## 8. 动态分段算法 [doc, E5 `re/chm_idman/Segmentation.htm`]

厂商随包文档原话（观测到的规格说明）：

- "Dynamic file segmentation that uses in-half division rule"
- "When new connection becomes available IDM finds the largest segment to download and divide it in half."
- "Full reuse of connections that have finished downloading of assigned segments without additional connect and login stages"
- "IDM reassignes the segment to the first connection" if the next connection hasn't started yet
- "IDM won't divide the segment only when its size is too small for this connection type"
- "IDM saves all file positions several times per minute. Thus after you stopped/paused downloading or even after unexpected power shutoff, IDM will resume download and assemble file correctly."
- 进度条语义：粉线=分段起点，蓝色=已下载，白色=待下载

## 9. CLI 兼容面 [doc, E5 `re/chm_idman/Command_Line.htm`]

```
idman /s
idman /d URL [/p local_path] [/f local_file_name] [/q] [/h] [/n] [/a]
  /d URL  下载
  /s      启动调度队列
  /p      保存目录
  /f      保存文件名
  /q      下载完成后退出
  /h      下载完成后挂断
  /n      静默模式
  /a      只加入队列不开始
```

## 10. 站点抓取器 [doc, E5 `re/chm_grab/*`]

- 四步向导：起始页 → 链接层级 → 位置/域 → 文件过滤器
- 过滤器支持 `*` 通配、多类型逗号分隔、`<start page>` 占位
- 去重判据：**同名且同大小**
- 高级项：include/exclude 路径与域、文件大小上下限
- 项目文件 `%sproject%s.igp` / `%sproject%s.gsd`，目录 `%sGrabber\Projects\`

## 11. 其它 [obs]

- `Accept-Language: en-US` 默认
- 观测到的 User-Agent 片段：`Mozilla/5.0 (compatible; MSIE 10.0; Windows NT 6.1; Trident/6.0)`、`Mozilla/5.0 (Windows NT 6.1; WOW64) AppleWebKit/537.36 (KHTML, like Gecko)`
- 限速/配额：`Options->Connection` 的 Download Limits（"40MB/hour or not more than 150MB every 4 hours"）[doc]
- 休眠/关机/挂断等完成动作 [obs 8905–9022]

---

## 未解项（不假装已知）

1. **函数级实现细节未知**：没有反编译证据，分段调度器的具体数据结构、重试退避曲线、连接复用的握手细节都只有行为级规格（§8）与错误文案（§6）支撑。
2. **native messaging 报文体未知**：`background.js` 被压缩，报文是自定义紧凑序列化（`MSG#<l>#<type>#<A>#<j>` 加 `,<idx>=<type>:<val>`），我没有完整还原全部消息号语义。
3. **加密字段编码未知**：`HttpEncPassword` / `SocksPass` 等 `Enc` 后缀字段的密钥方案未观测到。
4. **网络驱动**：`idmtdi*.sys` / `idmwfp*.sys`（TDI + WFP callout）用于透明接管，复刻版本不实现内核驱动，改用浏览器扩展 + 本地桥接，属于**设计选择而非观测结论**。
5. `IDFType.dat` / `idmfc.dat` 的二进制格式未解析，文件类型表来自文档与字符串。

## 复刻映射

| IDM 观测行为 | 复刻实现 |
|---|---|
| §2 手写 HTTP/1.1 + Range 模板 | `internal/httpx`：自己拼报文，不走 `net/http` 的高层默认头 |
| §8 对半分割动态分段 | `internal/engine` 段表 + `splitLargest()` |
| §5 `%s.tmp` + 多次/分钟存盘 | `.odm` 元数据 + `.odm.part` 稀疏临时文件，1s 落盘节流 |
| §6 超时重连 / 降级单连接 | `retryPolicy` + `degradeToSingle()` |
| §4 配置键名与代理体系 | `internal/config`，键名对齐 |
| §7 扩展 + native messaging | `extension/` + `odm.exe --native-host`（自定 JSON 报文，见 README） |
| §9 CLI | `cmd/odm` 参数解析，同形 |
| §10 抓取器 | `internal/grabber` |
