/**
 * 复制文本到剪贴板。
 *
 * 面板常通过 http://IP 访问（非安全上下文），此时 navigator.clipboard 不可用，
 * 直接调用会静默失败或抛错。这里优先使用 async Clipboard API（HTTPS/localhost），
 * 不可用时自动降级为 textarea + document.execCommand('copy')（http 环境仍可用）。
 *
 * @returns 是否复制成功
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  if (text == null) return false;
  // 优先：安全上下文下的 async Clipboard API
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 继续走降级路径
    }
  }
  // 降级：临时 textarea + execCommand（http://IP 环境的主要路径）
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0;width:1px;height:1px';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}
