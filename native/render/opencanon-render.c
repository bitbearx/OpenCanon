/* SPDX-License-Identifier: GPL-2.0-or-later
 * Copyright (C) 2026 OpenCanon contributors
 *
 * 本辅助程序直接链接 GPL Gutenprint，按 GPL-2.0-or-later 分发，无任何担保。
 * 许可全文见 COPYING；来源及分发要求见 NOTICE。
 * API 用法依据 Gutenprint include/gutenprint/{image,printers,vars}.h 与
 * src/testpattern/testpattern.c；参数依据 src/main/print-canon.c、
 * generic-options.c、canon-media.h 与 src/xml/papers/standard.xml。
 * https://github.com/echiu64/gutenprint
 * 本文件独立实现客户端适配，不包含 Gutenprint 驱动实现的复制代码。
 */
#define _POSIX_C_SOURCE 200809L
#define _FILE_OFFSET_BITS 64
#include <gutenprint/gutenprint.h>
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <unistd.h>

#define MAX_EDGE 4200
#define MAX_RASTER ((uint64_t)64 * 1024 * 1024)
#define MAX_JOB ((uint64_t)1024 * 1024 * 1024)
#define DRIVER "bjc-E560-series"

/* 纸张映射使用 Gutenprint 实际枚举；无对应标准尺寸的洋形信封用 Custom。 */
typedef struct {
    const char *id, *gutenprint;
    double width, height;
} paper_t;
static const paper_t papers[] = {
    {"A4", "A4", 210, 297}, {"A5", "A5", 148, 210},
    {"JIS-B5", "B5", 182, 257}, {"Letter", "Letter", 215.9, 279.4},
    {"Legal", "Legal", 215.9, 355.6}, {"4x6", "w288h432", 101.6, 152.4},
    {"5x7", "w360h504", 127, 177.8}, {"8x10", "c8x10", 203.2, 254},
    {"L", "w252" "h360J", 89, 127}, {"Postcard", "w283" "h420", 100, 148},
    {"DoublePostcard", "w420" "h567", 200, 148}, {"Env10", "COM10", 104.8, 241.3},
    {"EnvDL", "DL", 110, 220}, {"EnvYou4", "Custom", 105, 235},
    {"EnvYou6", "Custom", 98, 190}
};

/* 作业选项仅包含对外约定，内部参数名称由配置函数转换。 */
typedef struct {
    const paper_t *paper;
    const char *media_type, *color, *duplex;
    double width, height;
    int copies, count;
    char **paths;
} options_t;

/* 页面只持有一个文件与行元数据，空白背面不分配整页图像。 */
typedef struct {
    FILE *file;
    off_t offset;
    int width, height, blank, failed, mono;
} page_t;

/* 输出状态记录实际设备流长度和写入失败，回调错误不会被忽略。 */
typedef struct {
    uint64_t bytes;
    int failed;
} output_t;

/* fail 输出固定诊断并终止；调用方必须丢弃任何部分设备流。 */
static void fail(const char *message)
{
    fprintf(stderr, "opencanon-render: %s\n", message);
    exit(EXIT_FAILURE);
}

/* limit_resource 保留更严格的外部限制，设置失败时停止处理。 */
static void limit_resource(int resource, rlim_t maximum)
{
    struct rlimit limit;
    if (getrlimit(resource, &limit) != 0) fail("cannot read resource limits");
    if (limit.rlim_cur == RLIM_INFINITY || limit.rlim_cur > maximum)
        limit.rlim_cur = maximum;
    if (setrlimit(resource, &limit) != 0) fail("cannot apply resource limits");
}

/* resource_limits 同时约束内存、文件、CPU 与文件描述符。 */
static void resource_limits(int rasterize)
{
    limit_resource(RLIMIT_AS, (rlim_t)1024 * 1024 * 1024);
    limit_resource(RLIMIT_FSIZE, (rlim_t)(rasterize ? MAX_RASTER : MAX_JOB));
    limit_resource(RLIMIT_CPU, rasterize ? 90 : 1200);
    limit_resource(RLIMIT_NOFILE, 256);
    limit_resource(RLIMIT_CORE, 0);
}

/* integer 严格解析有界正整数，拒绝符号、溢出和尾随内容。 */
static int integer(const char *text, int maximum)
{
    char *end;
    if (!text[0] || !isdigit((unsigned char)text[0])) fail("invalid integer");
    errno = 0;
    long value = strtol(text, &end, 10);
    if (errno || *end || value < 1 || value > maximum) fail("integer out of range");
    return (int)value;
}

/* dimension 拒绝非有限、非正或过大毫米尺寸。 */
static double dimension(const char *text)
{
    char *end;
    errno = 0;
    double value = strtod(text, &end);
    if (errno || end == text || *end || !isfinite(value) || value <= 0 || value > 400)
        fail("invalid paper dimension");
    return value;
}

/* find_paper 只接受公开纸张标识，不接受任意驱动参数。 */
static const paper_t *find_paper(const char *id)
{
    for (size_t i = 0; i < sizeof(papers)/sizeof(papers[0]); ++i)
        if (!strcmp(id, papers[i].id)) return &papers[i];
    fail("unknown paper identifier");
    return NULL;
}

/* parse_options 校验所有必需参数、页数和双面介质约束。 */
static options_t parse_options(int argc, char **argv)
{
    options_t options = {0};
    unsigned seen = 0;
    int i;
    for (i = 2; i < argc && strcmp(argv[i], "--"); i += 2) {
        unsigned bit = 0;
        if (i + 1 >= argc) fail("missing option value");
        const char *value = argv[i + 1];
        if (!strcmp(argv[i], "--media")) {
            bit = 1; options.paper = find_paper(value);
        } else if (!strcmp(argv[i], "--media-type")) {
            bit = 2; options.media_type = value;
        } else if (!strcmp(argv[i], "--color")) {
            bit = 4; options.color = value;
        } else if (!strcmp(argv[i], "--duplex")) {
            bit = 8; options.duplex = value;
        } else if (!strcmp(argv[i], "--copies")) {
            bit = 16; options.copies = integer(value, 99);
        } else if (!strcmp(argv[i], "--width-mm")) {
            bit = 32; options.width = dimension(value);
        } else if (!strcmp(argv[i], "--height-mm")) {
            bit = 64; options.height = dimension(value);
        } else fail("unknown option");
        if (seen & bit) fail("duplicate option");
        seen |= bit;
    }
    if (seen != 127 || i >= argc) fail("all render options and -- are required");
    options.count = argc - i - 1;
    options.paths = argv + i + 1;
    if (options.count < 1) fail("at least one page is required");
    if (strcmp(options.color, "color") && strcmp(options.color, "mono")) fail("invalid color mode");
    if (strcmp(options.duplex, "single") && strcmp(options.duplex, "long") && strcmp(options.duplex, "short"))
        fail("invalid duplex mode");
    if (strcmp(options.media_type, "plain") && strcmp(options.media_type, "photo") &&
        strcmp(options.media_type, "postcard") && strcmp(options.media_type, "envelope"))
        fail("invalid media type");
    if (fabs(options.width-options.paper->width) > 0.2 || fabs(options.height-options.paper->height) > 0.2)
        fail("paper dimensions do not match identifier");
    int duplex = strcmp(options.duplex, "single") != 0;
    if (duplex && (strcmp(options.media_type, "plain") ||
        (strcmp(options.paper->id, "A4") && strcmp(options.paper->id, "Letter"))))
        fail("automatic duplex requires A4 or Letter plain paper");
    return options;
}

/* set_choice 向已加载型号查询真实枚举，拒绝缺失选项而不静默回退。 */
static void set_choice(stp_vars_t *vars, const char *name, const char *value)
{
    stp_parameter_t description;
    stp_describe_parameter(vars, name, &description);
    int valid = description.p_type == STP_PARAMETER_TYPE_STRING_LIST &&
        description.is_active && description.bounds.str &&
        stp_string_list_is_present(description.bounds.str, value);
    stp_parameter_description_destroy(&description);
    if (!valid) {
        fprintf(stderr, "opencanon-render: %s does not support %s=%s\n", DRIVER, name, value);
        exit(EXIT_FAILURE);
    }
    stp_set_string_parameter(vars, name, value);
}

/* write_output 直接写真实 Canon 数据，超过输出上限或短写立即标记失败。 */
static void write_output(void *data, const char *buffer, size_t size)
{
    output_t *output = data;
    if (output->failed) return;
    if (size > MAX_JOB-output->bytes || fwrite(buffer, 1, size, stdout) != size) {
        output->failed = 1;
        return;
    }
    output->bytes += size;
}

/* write_error 将 Gutenprint 诊断与二进制 stdout 分离。 */
static void write_error(void *data, const char *buffer, size_t size)
{
    (void)data;
    (void)fwrite(buffer, 1, size, stderr);
}

/* configure 设置 E560、非无边距、作业模式以及实际介质枚举。 */
static stp_vars_t *configure(const stp_printer_t *printer, const options_t *options, output_t *output)
{
    stp_vars_t *vars = stp_vars_create();
    if (!vars) fail("cannot allocate Gutenprint settings");
    stp_set_printer_defaults(vars, printer);
    stp_set_outfunc(vars, write_output);
    stp_set_outdata(vars, output);
    stp_set_errfunc(vars, write_error);
    set_choice(vars, "JobMode", "Job");
    set_choice(vars, "InputImageType", "RGB");
    set_choice(vars, "ChannelBitDepth", "8");
    set_choice(vars, "InputSlot", "Front");
    set_choice(vars, "PageSize", options->paper->gutenprint);
    stp_set_page_width(vars, options->width * 72.0 / 25.4);
    stp_set_page_height(vars, options->height * 72.0 / 25.4);
    const char *media = "Plain";
    if (!strcmp(options->media_type, "photo")) media = "PhotoPlusGloss2";
    else if (!strcmp(options->media_type, "postcard")) media = "Hagaki";
    else if (!strcmp(options->media_type, "envelope")) media = "Envelope";
    set_choice(vars, "MediaType", media);
    set_choice(vars, "PrintingMode", !strcmp(options->color, "mono") ? "BW" : "Color");
    set_choice(vars, "Duplex", !strcmp(options->duplex, "single") ? "None" :
        (!strcmp(options->duplex, "long") ? "DuplexNoTumble" : "DuplexTumble"));
    set_choice(vars, "Quality", "Standard");
    set_choice(vars, "Orientation", "Portrait");
    stp_set_boolean_parameter(vars, "FullBleed", 0);
    return vars;
}

/* ppm_token 有界读取 PPM 头；注释与 CRLF 不会误吞首个像素。 */
static void ppm_token(FILE *file, char *token, size_t capacity)
{
    int c;
    size_t used = 0;
    for (;;) {
        c = fgetc(file);
        if (c == '#') {
            while ((c = fgetc(file)) != '\n' && c != EOF) {
                if (ftello(file) > 4096) fail("PPM header is too large");
            }
        }
        if (c == EOF) fail("truncated PPM header");
        if (!isspace((unsigned char)c)) break;
        if (ftello(file) > 4096) fail("PPM header is too large");
    }
    do {
        if (used + 1 >= capacity || ftello(file) > 4096) fail("invalid PPM token");
        token[used++] = (char)c;
        c = fgetc(file);
    } while (c != EOF && !isspace((unsigned char)c));
    if (c == EOF) fail("truncated PPM header");
    if (c == '\r') {
        c = fgetc(file);
        if (c != '\n' && c != EOF) ungetc(c, file);
    }
    token[used] = 0;
}

/* open_page 仅接受单张 8 位 RGB P6 常规文件，并核实精确像素长度。 */
static page_t open_page(const char *path)
{
    page_t page = {0};
    int fd = open(path, O_RDONLY | O_NOFOLLOW | O_NONBLOCK);
    if (fd < 0) fail("cannot open PPM input");
    struct stat info;
    if (fstat(fd, &info) != 0 || !S_ISREG(info.st_mode) || info.st_size <= 0 ||
        (uint64_t)info.st_size > MAX_RASTER) fail("invalid PPM file size or type");
    page.file = fdopen(fd, "rb");
    if (!page.file) fail("cannot read PPM input");
    char token[32];
    ppm_token(page.file, token, sizeof(token));
    if (strcmp(token, "P6")) fail("PPM must use binary P6 RGB");
    ppm_token(page.file, token, sizeof(token));
    page.width = integer(token, MAX_EDGE);
    ppm_token(page.file, token, sizeof(token));
    page.height = integer(token, MAX_EDGE);
    ppm_token(page.file, token, sizeof(token));
    if (strcmp(token, "255")) fail("PPM must have 8-bit channels");
    page.offset = ftello(page.file);
    uint64_t expected = (uint64_t)page.width * page.height * 3;
    if (page.offset < 0 || (uint64_t)info.st_size != (uint64_t)page.offset+expected)
        fail("PPM pixel data is truncated or has trailing bytes");
    return page;
}

/* scratch_file 在调用方指定目录创建并立即解除路径，进程退出自动释放磁盘。 */
static FILE *scratch_file(void)
{
    const char *directory = getenv("TMPDIR");
    if (!directory || !directory[0]) directory = "/tmp";
    char path[PATH_MAX];
    int length = snprintf(path, sizeof(path), "%s/opencanon-rotate-XXXXXX", directory);
    if (length < 0 || (size_t)length >= sizeof(path)) fail("scratch directory path too long");
    int fd = mkstemp(path);
    if (fd < 0) fail("cannot create rotation scratch file");
    if (unlink(path)) { close(fd); fail("cannot unlink rotation scratch file"); }
    FILE *file = fdopen(fd, "w+b");
    if (!file) { close(fd); fail("cannot open rotation scratch stream"); }
    return file;
}

/* rotate_page 以小块转置到匿名临时文件，保持行读取与固定内存占用。 */
static void rotate_page(page_t *page)
{
    FILE *rotated = scratch_file();
    unsigned char tile[64][64][3], row[64*3];
    int width = page->width, height = page->height;
    for (int y = 0; y < height; y += 64) {
        int rows = height-y < 64 ? height-y : 64;
        for (int x = 0; x < width; x += 64) {
            int columns = width-x < 64 ? width-x : 64;
            for (int r = 0; r < rows; ++r) {
                off_t offset = page->offset + ((off_t)(y+r)*width+x)*3;
                if (fseeko(page->file, offset, SEEK_SET) ||
                    fread(tile[r], 3, (size_t)columns, page->file) != (size_t)columns)
                    fail("cannot read rotation tile");
            }
            for (int c = 0; c < columns; ++c) {
                for (int r = 0; r < rows; ++r)
                    memcpy(row+r*3, tile[rows-1-r][c], 3);
                off_t offset = ((off_t)(x+c)*height + height-y-rows)*3;
                if (fseeko(rotated, offset, SEEK_SET) ||
                    fwrite(row, 3, (size_t)rows, rotated) != (size_t)rows)
                    fail("cannot write rotation tile");
            }
        }
    }
    if (fflush(rotated)) fail("cannot flush rotation scratch file");
    fclose(page->file);
    page->file = rotated;
    page->offset = 0;
    page->width = height;
    page->height = width;
}

/* image_init 为每页清除回调错误状态。 */
static void image_init(stp_image_t *image)
{
    page_t *page = image->rep;
    page->failed = 0;
}

/* image_width 返回输入像素宽度。 */
static int image_width(stp_image_t *image)
{
    return ((page_t *)image->rep)->width;
}

/* image_height 返回输入像素高度。 */
static int image_height(stp_image_t *image)
{
    return ((page_t *)image->rep)->height;
}

/* image_row 按需读取一行，空白背面生成同样有效的 RGB 图像。 */
static stp_image_status_t image_row(stp_image_t *image, unsigned char *data, size_t limit, int row)
{
    page_t *page = image->rep;
    size_t bytes = (size_t)page->width*3;
    if (limit < bytes || row < 0 || row >= page->height) {
        page->failed = 1;
        return STP_IMAGE_STATUS_ABORT;
    }
    if (page->blank) memset(data, 255, bytes);
    else if (fseeko(page->file, page->offset+(off_t)row*(off_t)bytes, SEEK_SET) ||
        fread(data, 1, bytes, page->file) != bytes) {
        page->failed = 1;
        return STP_IMAGE_STATUS_ABORT;
    }
    /* 照片模式可能使用复合彩墨；输入先灰度化，保证单色作业不保留彩色像素。 */
    if (page->mono && !page->blank) {
        for (size_t i = 0; i < bytes; i += 3) {
            unsigned char gray = (unsigned char)((77U*data[i]+150U*data[i+1]+29U*data[i+2]+128U) >> 8);
            data[i] = data[i+1] = data[i+2] = gray;
        }
    }
    return STP_IMAGE_STATUS_OK;
}

/* image_appname 提供驱动可嵌入的应用名称。 */
static const char *image_appname(stp_image_t *image)
{
    (void)image;
    return "OpenCanon";
}

/* image_conclude 的文件生命周期由调用者管理，不在库回调中关闭。 */
static void image_conclude(stp_image_t *image)
{
    (void)image;
}

/* fit_page 选择最佳旋转并按比例居中到有边距可打印区域。 */
static void fit_page(stp_vars_t *vars, page_t *page)
{
    stp_dimension_t left, right, top, bottom;
    stp_get_imageable_area(vars, &left, &right, &bottom, &top);
    /* 驱动严格比较边界；向可打印区域内部取相邻浮点值，吸收毫米换算误差。 */
    left = nextafter(left, right);
    right = nextafter(right, left);
    top = nextafter(top, bottom);
    bottom = nextafter(bottom, top);
    double available_width = right-left, available_height = bottom-top;
    if (available_width <= 0 || available_height <= 0) fail("invalid imageable area");
    double scale = fmin(available_width/page->width, available_height/page->height);
    double rotated = fmin(available_width/page->height, available_height/page->width);
    if (!page->blank && rotated > scale) {
        rotate_page(page);
        scale = rotated;
    }
    /* 除法及像素乘法可能将恰好贴边的尺寸向上舍入，统一缩放向下取值。 */
    scale = nextafter(scale, 0.0);
    double width = page->width*scale, height = page->height*scale;
    stp_set_width(vars, width);
    stp_set_height(vars, height);
    stp_set_left(vars, left+(available_width-width)/2);
    stp_set_top(vars, top+(available_height-height)/2);
}

/* print_page 为每个输出面设置从零起始页号，避免重复初始化双面作业。 */
static void print_page(stp_vars_t *vars, page_t *page, int number, output_t *output)
{
    stp_image_t image = {
        image_init, image_init, image_width, image_height,
        image_row, image_appname, image_conclude, page
    };
    fit_page(vars, page);
    stp_set_int_parameter(vars, "PageNumber", number);
    if (!stp_verify(vars)) fail("Gutenprint rejected page settings");
    if (number == 0 && stp_start_job(vars, &image) != 1) fail("Gutenprint could not start job");
    if (stp_print(vars, &image) != 1 || page->failed || output->failed)
        fail("Gutenprint page rendering failed");
}

/* check_driver 验证精确型号与必要枚举，不用相似型号替代 E560。 */
static void check_driver(const stp_printer_t *printer)
{
    output_t output = {0};
    options_t options = {&papers[0], "plain", "color", "long", 210, 297, 1, 0, NULL};
    stp_vars_t *vars = configure(printer, &options, &output);
    page_t blank = {.width = 1, .height = 1, .blank = 1};
    fit_page(vars, &blank);
    stp_set_int_parameter(vars, "PageNumber", 0);
    if (!stp_verify(vars)) fail("Gutenprint default E560 configuration is invalid");
    set_choice(vars, "PrintingMode", "BW");
    set_choice(vars, "Duplex", "DuplexTumble");
    set_choice(vars, "Duplex", "None");
    for (size_t i = 0; i < sizeof(papers)/sizeof(papers[0]); ++i)
        set_choice(vars, "PageSize", papers[i].gutenprint);
    set_choice(vars, "MediaType", "PhotoPlusGloss2");
    set_choice(vars, "MediaType", "Hagaki");
    set_choice(vars, "MediaType", "Envelope");
    stp_vars_destroy(vars);
}

/* render_job 逐份循环全部页面，奇数双面在每份末尾编码真实空白背面。 */
static void render_job(const stp_printer_t *printer, const options_t *options)
{
    uint64_t total = 0;
    output_t output = {0};
    stp_vars_t *vars = configure(printer, options, &output);
    for (int i = 0; i < options->count; ++i) {
        page_t page = open_page(options->paths[i]);
        total += (uint64_t)page.width*page.height*3;
        if (fclose(page.file)) fail("cannot close PPM input");
        if (total > MAX_JOB) fail("total raster input exceeds 1 GiB");
    }
    int number = 0;
    for (int copy = 0; copy < options->copies; ++copy) {
        for (int i = 0; i < options->count; ++i) {
            page_t page = open_page(options->paths[i]);
            page.mono = !strcmp(options->color, "mono");
            print_page(vars, &page, number++, &output);
            if (fclose(page.file)) fail("cannot close PPM input");
        }
        if (strcmp(options->duplex, "single") && options->count % 2) {
            page_t blank = {.width = 1, .height = 1, .blank = 1};
            print_page(vars, &blank, number++, &output);
        }
    }
    page_t blank = {.width = 1, .height = 1, .blank = 1};
    stp_image_t image = {
        image_init, image_init, image_width, image_height,
        image_row, image_appname, image_conclude, &blank
    };
    if (stp_end_job(vars, &image) != 1 || output.failed || !output.bytes || fflush(stdout))
        fail("Gutenprint could not finish device output");
    stp_vars_destroy(vars);
}

/* main 提供环境检查、受限 Poppler 执行以及无需设备的 PPM 渲染入口。 */
int main(int argc, char **argv)
{
    if (argc >= 3 && !strcmp(argv[1], "--rasterize")) {
        resource_limits(1);
        execvp(argv[2], argv+2);
        fail("cannot execute pdftoppm");
    }
    if (argc < 2 || (strcmp(argv[1], "--check") && strcmp(argv[1], "--render")))
        fail("usage: opencanon-render --check | --render --media ID --media-type plain|photo|postcard|envelope --color color|mono --duplex single|long|short --copies N --width-mm MM --height-mm MM -- page.ppm ...");
    resource_limits(0);
    if (stp_init() != 0) fail("cannot initialize Gutenprint");
    const stp_printer_t *printer = stp_get_printer_by_driver(DRIVER);
    if (!printer || stp_printer_get_model(printer) != 12000560)
        fail("installed Gutenprint does not provide exact bjc-E560-series model 12000560");
    if (!strcmp(argv[1], "--check")) {
        if (argc != 2) fail("--check takes no arguments");
        check_driver(printer);
        puts(DRIVER " model=12000560 support=EXPERIMENTAL");
    } else {
        options_t options = parse_options(argc, argv);
        render_job(printer, &options);
    }
    return EXIT_SUCCESS;
}
