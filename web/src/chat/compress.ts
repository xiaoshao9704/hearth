// 发图前的本地压缩：截图动不动 4~8MB，原样走数据通道既慢又容易撞上限。
// 只处理位图（jpeg/png/webp）；GIF 会被 canvas 拍成第一帧，一律原样放过，
// 其它类型（视频、压缩包、文档）也原样放过——这里不做通用转码。
const COMPRESSIBLE = new Set(['image/jpeg', 'image/png', 'image/webp']);

// 触发阈值：小图重编码通常更大且更糊，不值当
const MIN_BYTES = 1.5 * 1024 * 1024;
const MAX_EDGE = 1920;
const QUALITY = 0.85;

let webpOk: boolean | null = null;

function supportsWebp(): boolean {
  if (webpOk === null) {
    const c = document.createElement('canvas');
    c.width = c.height = 1;
    webpOk = c.toDataURL('image/webp').startsWith('data:image/webp');
  }
  return webpOk;
}

// 换后缀保留原名：接收方看到的是「截图.webp」而不是名不副实的「截图.png」
function rename(name: string, mime: string): string {
  const ext = mime === 'image/webp' ? 'webp' : 'jpg';
  const dot = name.lastIndexOf('.');
  const base = dot > 0 ? name.slice(0, dot) : name;
  return `${base}.${ext}`;
}

// 压不动、压不小或压不了都返回原文件：这一步只允许改善结果，不允许把事情变糟
export async function compressImage(file: File): Promise<File> {
  if (!COMPRESSIBLE.has(file.type)) return file;
  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(file);
  } catch {
    return file; // 解不开（坏文件、浏览器不支持该子格式）：原样发，让服务端与对端去判断
  }
  try {
    const edge = Math.max(bitmap.width, bitmap.height);
    const scale = edge > MAX_EDGE ? MAX_EDGE / edge : 1;
    if (scale === 1 && file.size <= MIN_BYTES) return file;
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(bitmap.width * scale));
    canvas.height = Math.max(1, Math.round(bitmap.height * scale));
    const ctx = canvas.getContext('2d');
    if (!ctx) return file;
    ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
    const mime = supportsWebp() ? 'image/webp' : 'image/jpeg';
    const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, mime, QUALITY));
    if (!blob || blob.size >= file.size) return file;
    return new File([blob], rename(file.name, mime), { type: mime, lastModified: file.lastModified });
  } finally {
    bitmap.close();
  }
}
