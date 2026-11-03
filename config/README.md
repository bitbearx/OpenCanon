# Canon E560 能力记录

本机队列 `OpenCanon_E560` 使用 Canon E560 series CUPS Driver 16.91.0.0。现有默认队列 `_192_168_50_24` 保留，未修改系统默认打印机。

- 官方安装器：https://ph.canon/en/support/0101154501
- 实际 PPD：`/etc/cups/ppd/OpenCanon_E560.ppd`；ModelName 为 `Canon E560 series`。
- Canon 发现的设备 URI：`canonijnetwork://Canon/Canon%20E560%20series?TYPE=_canon-bjnp1._tcp.`。
- Bonjour `_canon-bjnp1._tcp` 将该设备解析到 `AE9346000000.local.:8611`，IPv4 为 `192.168.50.24`，MAC 为 `50:03:CF:AE:93:46`；设备网页型号与固件为 E560 series／1.060。
- 上述 IP 是安装时的实测记录；运行页面以 Bonjour 扫描结果为准，保存到应用数据目录的 `printer-discovery.json`，点击“重新扫描”更新。队列 URI 不绑定这个数字地址。
- `lpstat -a OpenCanon_E560` 确认队列接受任务；此状态不证明设备在线或纸张已经输出。

`printer.json` 所有 options 的键和值均逐项匹配实际 PPD。PageSize 对应纸张尺寸；`CNIJMediaType=0` 普通纸，`50` Photo Paper Plus Glossy II，`7` 明信片，`8` 信封。`CNIJMarginType=0` 为驱动默认值，配置不提供无边距尺寸。照片纸选项要求装入相应纸张，上传照片也可选择 A4 普通纸。

色彩同时设置 `CNIJGrayScale` 与 `CNIJGrayScaleCheckBox`：0 为彩色，1 为灰度。实际用墨由打印机决定。双面使用 `Duplex=None`、`DuplexNoTumble`、`DuplexTumble`。业务 ID 不作为命令行驱动参数。

B5 的 PPD 标签为 JIS B5，PaperDimension 为 515.91 × 728.50 pt，即约 182 × 257 mm；不提供 ISO B5 的错误尺寸。

PPD 的 UIConstraints 禁止非普通纸与双面组合。佳能 E560 手册还明确双面仅支持 A4／Letter，因此配置将其他纸张与长边／短边的组合逐个列入 constraints（包括彩色和单色）：https://ij.manual.canon/ij/webmanual/PrinterDriver/W/E560%20series/1.0/EN/PPG/dg-c_duplex.html 。

这里记录的是驱动枚举、设备发现与提交能力。正反方向、实际灰度／彩色、纸张和逐份排序须以实物验收为准；其他尺寸不应在没有对应纸张时强行测试。驱动、队列或设备更换后，应重新核对本文件及 JSON 映射。
