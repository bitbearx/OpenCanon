# Canon E560 独立编码能力

运行时使用 Gutenprint 精确型号 **`bjc-E560-series` / model `12000560`**，没有使用相似型号代替，没有 Canon 专有二进制库，也没有 CUPS 服务或 macOS 驱动调用。上游支持级别为 **EXPERIMENTAL**，不能将软件枚举或网络 ACK 当作实物验收。

## 编码依据

- [Gutenprint 型号表](https://github.com/echiu64/gutenprint/blob/master/src/xml/printers/canon.xml)：`Canon E560 series` 对应上述精确 driver/model，并标明 EXPERIMENTAL。
- [E560 设备能力](https://github.com/echiu64/gutenprint/blob/master/src/main/canon-printers.h)：精确 E560 条目及其纸张、双面和命令能力。
- [E560 分辨率和颜色模式](https://github.com/echiu64/gutenprint/blob/master/src/main/canon-modes.h)：该型号独立的打印模式列表。
- [Canon 编码实现](https://github.com/echiu64/gutenprint/blob/master/src/main/print-canon.c)：XML 作业头尾、ESC 命令和压缩栅格输出。
- [Canon 官方 E560 双面说明](https://ij.manual.canon/ij/webmanual/PrinterDriver/W/E560%20series/1.0/EN/PPG/dg-c_duplex.html)：自动双面限 A4／Letter 普通纸。

已调查的 Canon `cnijfilter` Linux 包也列出 e560 / 437，但其核心编码库是专有二进制，不适合本次开源、原生 ARM64 路线，因此未采用。见 [型号表](https://github.com/endlessm/cnijfilter-common)、[437 库目录](https://github.com/endlessm/cnijfilter-common/tree/master/437) 和 [许可附录](https://github.com/endlessm/cnijfilter-common/blob/master/LICENSE-cnijfilter-4.10EN.txt)。

辅助程序从 Debian 的 `5.3.4.20220624T01008808d602` 原始源包构建并静态链接 Gutenprint，校验值及补丁随 `native/render` 构建材料提供。补丁修正 `buffer-image.c` 为行指针数组预留完整 NULL 哨兵；原库在 64 位平台处理 1×1 双面空白页时会越界读取并可能释放无效指针。该修复保留原始图像尺寸和设备编码逻辑。

## 配置格式

`printer.json` 保存 `model`、纸张 ID/尺寸/介质、颜色、双面与禁止组合。旧的 `queue`、PPD `options`、`CNIJMediaType`、`CNIJGrayScale` 不再用于执行。`native/render` 将业务值映射为 Gutenprint 的实际参数，并查询安装版本枚举验证；缺型号或参数时明确失败。

| 业务纸张 | Gutenprint PageSize | 毫米尺寸 | 介质 |
|---|---|---|---|
| A4 | A4 | 210 × 297 | plain |
| A5 | A5 | 148 × 210 | plain |
| JIS-B5 | B5 | 182 × 257 | plain |
| Letter | Letter | 215.9 × 279.4 | plain |
| Legal | Legal | 215.9 × 355.6 | plain |
| 4x6 | w288h432 | 101.6 × 152.4 | photo |
| 5x7 | w360h504 | 127 × 177.8 | photo |
| 8x10 | c8x10 | 203.2 × 254 | photo |
| L | `w252` + `h360J`（拼接） | 89 × 127 | photo |
| Postcard | `w283` + `h420`（拼接） | 100 × 148 | postcard |
| DoublePostcard | `w420` + `h567`（拼接） | 200 × 148 | postcard |
| Env10 | COM10 | 104.8 × 241.3 | envelope |
| EnvDL | DL | 110 × 220 | envelope |
| EnvYou4 | Custom | 105 × 235 | envelope |
| EnvYou6 | Custom | 98 × 190 | envelope |

介质 `plain/photo/postcard/envelope` 分别对应 `Plain/PhotoPlusGloss2/Hagaki/Envelope`。普通纸与照片纸不是按上传文件类型自动选择，用户必须装入对应纸张。关闭无边距，使用前方进纸和标准质量；不把已有 PPD 选项直接转发给设备。

颜色 `color/mono` 对应 `Color/BW`；单色输入会先灰度化，实际用墨取决于设备及 Gutenprint 模式。双面 `single/long/short` 对应 `None/DuplexNoTumble/DuplexTumble`。非 A4／Letter 普通纸的自动双面在服务端和辅助程序中均拒绝。

## 设备通信

Go BJNP 协议事实依据固定版本的 [cups-bjnp](https://github.com/bitwiseworks/cups-bjnp-os2/tree/5f1a04542c5e1a714344563972cd1914b4a19185)：

- 16 字节大端头；UDP 0x30 身份、0x20 状态、0x10 建立会话、0x11 结束会话；TCP 0x21 逐块传输。
- E560 实机建立会话响应包含 4 字节不透明扩展；接受空负载或该扩展长度，会话号只取已校验响应头，不猜扩展内容含义。
- 单个未确认帧、响应命令/序号/会话/长度校验；设备明确确认零接收时有限流控，未知接收结果不自动重发。
- 保留公开后端的 BJL 标记分帧兼容行为，包括标记跨输入块的情况。
- 支持旧式 `BST` 及实测 Canon XML `GetStatusResponse`。XML 严格校验命名空间、打印服务、成功响应、唯一节点、`BSCCe/ReadData` 和字节数；空闲仍必须通过 BST、耗材及错误码检查。已确认的打印中状态禁止新提交，未知或截断状态保持不可用。
- 没有可证明本作业物理完成的查询。设备接收数据、设备当前忙和真实出纸是不同事实。
- 实机缓冲区满会持续返回完整零 ACK；服务将每帧流控等待限制为 2 分钟，单次网络交互仍限制为 15 秒。该等待只适用于明确零接收，丢 ACK、部分接收或已结束的不完整任务不得自动重发。

原先安装记录中，E560 Bonjour 名称为 `Canon E560 series`，主机为 `AE9346000000.local.`，MAC 为 `50:03:CF:AE:93:46`，地址为 `192.168.50.24`，服务端口 8611。这些只用于核对原设备；运行时必须直接查询身份和状态，不能把历史地址当作在线证明。

## 许可与验收

Gutenprint 与本项目 C 辅助程序使用 GPL-2.0-or-later。辅助程序源代码、构建规则和许可随镜像提供，第三方库的对应源码分发要求见 [native/render/NOTICE](../native/render/NOTICE)。Go BJNP 实现没有直接翻译上游 C 源码，也未重许可整个 Go 项目。

已用 Debian bookworm ARM64 的真实 Gutenprint 检查精确型号并生成设备流；E560 实机已通过 A4 普通纸、彩色、长边双面、两页一份测试，用户确认一张纸正反面完整、颜色与方向正常。其他纸张、短边双面、单色及多份尚未逐项完成实物验收。详情见 [README](../README.md#验证边界)。更换机型、编码器版本或设备后，重新核对本文件、JSON 和辅助程序，不得仅改显示名称就宣称支持。
