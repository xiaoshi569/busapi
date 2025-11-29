const puppeteer = require('puppeteer');
const axios = require('axios');
const fs = require('fs').promises;
const path = require('path');

// 解析命令行参数
function parseArgs() {
    const args = process.argv.slice(2);
    const config = {
        headless: false,
        threads: 1,
        continuous: false,
        dataDir: null,  // 自定义数据目录
        proxy: null,    // 代理地址
        quiet: false    // 静默模式
    };

    for (let i = 0; i < args.length; i++) {
        const arg = args[i];
        switch (arg) {
            case '--headless':
            case '-h':
                config.headless = true;
                break;
            case '--threads':
            case '-t':
                config.threads = parseInt(args[++i]) || 1;
                break;
            case '--continuous':
            case '-c':
                config.continuous = true;
                break;
            case '--data-dir':
            case '-d':
                config.dataDir = args[++i];
                break;
            case '--proxy':
            case '-p':
                config.proxy = args[++i];
                break;
            case '--quiet':
            case '-q':
                config.quiet = true;
                break;
            case '--help':
                console.log(`
用法: node main.js [选项]

选项:
  --headless, -h       无头模式运行
  --threads, -t <n>    线程数 (默认: 1)
  --continuous, -c     持续运行模式
  --data-dir, -d <dir> 数据保存目录
  --proxy, -p <url>    代理地址
  --quiet, -q          静默模式
  --help               显示帮助
                `);
                process.exit(0);
        }
    }

    return config;
}

// 读取配置（命令行参数优先）
async function loadConfig() {
    // 先解析命令行参数
    const cliConfig = parseArgs();
    
    // 如果有命令行参数，直接使用
    if (process.argv.length > 2) {
        return cliConfig;
    }

    // 否则尝试读取配置文件
    try {
        const configPath = path.join(__dirname, 'config.json');
        const configData = await fs.readFile(configPath, 'utf8');
        const fileConfig = JSON.parse(configData);
        return { ...cliConfig, ...fileConfig };
    } catch (error) {
        return cliConfig;
    }
}

// 确保 data 目录存在
async function ensureDataDir(customDir = null) {
    const dataDir = customDir || path.join(__dirname, 'data');
    try {
        await fs.access(dataDir);
    } catch {
        await fs.mkdir(dataDir, { recursive: true });
    }
    return dataDir;
}

// 全局静默模式
let quietMode = false;

// 获取临时邮箱
async function getTemporaryEmail(threadId) {
    if (!quietMode) console.log(`[线程 ${threadId}] 正在获取临时邮箱...`);
    const response = await axios.get('https://mail.chatgpt.org.uk/api/generate-email', {
        headers: {
            'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0',
            'referer': 'https://mail.chatgpt.org.uk'
        }
    });

    const email = response.data.email || response.data.data?.email;
    if (!quietMode) console.log(`[线程 ${threadId}] 获取到邮箱:`, email);
    return email;
}

// 获取邮件内容
async function getEmailContent(email, threadId, maxRetries = 20) {


    for (let i = 0; i < maxRetries; i++) {
        try {
            const response = await axios.get(`https://mail.chatgpt.org.uk/api/emails?email=${email}`, {
                headers: {
                    'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0',
                    'referer': 'https://mail.chatgpt.org.uk'
                }
            });

            if (response.data.success && response.data.data.emails.length > 0) {
                //   if (!quietMode) console.log(`[线程 ${threadId}] 成功获取邮件`);
                return response.data.data.emails[0];
            }
        } catch (error) {
            if (!quietMode) console.log(`[线程 ${threadId}] 尝试 ${i + 1}/${maxRetries} 失败:`, error.message);
        }

        // 等待 5 秒后重试
        if (!quietMode) console.log(`[线程 ${threadId}] 等待 5 秒后重试... (${i + 1}/${maxRetries})`);
        await new Promise(resolve => setTimeout(resolve, 5000));
    }

    throw new Error('无法获取邮件内容');
}

// 从邮件内容中提取验证码
function extractVerificationCode(emailContent, threadId) {
    const content = emailContent.content || '';
    const subject = emailContent.subject || '';
    // 常见的非验证码单词列表
    const commonWords = ['VERIFY', 'GOOGLE', 'UPDATE', 'MOBILE', 'DEVICE', 'SUBMIT', 'RESEND', 'CANCEL', 'DELETE', 'REMOVE', 'SEARCH', 'VIDEOS', 'IMAGES', 'GMAIL', 'EMAIL', 'ACCOUNT', 'CHROME'];

    // 方法1: 查找所有 6 位大写字母或数字的组合
    const matches = content.match(/\b[A-Z0-9]{6}\b/g);

    if (matches) {
        // 优先寻找包含数字的验证码 (如 F7W96C)
        const withDigits = matches.find(code =>
            !commonWords.includes(code) && /[0-9]/.test(code)
        );
        if (withDigits) {
            if (!quietMode) console.log(`[线程 ${threadId}] 选择包含数字的验证码: ${withDigits}`);
            return withDigits;
        }

        // 如果没有包含数字的，返回第一个非常见单词的匹配
        const anyMatch = matches.find(code => !commonWords.includes(code));
        if (anyMatch) {
            if (!quietMode) console.log(`[线程 ${threadId}] 选择第一个非常见词验证码: ${anyMatch}`);
            return anyMatch;
        }
    }

    // 方法2: 查找 "code" 附近的 6 位字符
    const contextMatch = content.match(/code\s*[:is]\s*([A-Z0-9]{6})/i);
    if (contextMatch) {
        if (!quietMode) console.log(`[线程 ${threadId}] 通过上下文找到验证码: ${contextMatch[1]}`);
        return contextMatch[1];
    }

    // 方法3: 查找 "verification" 附近的代码
    const verifyMatch = content.match(/verification\s*code\s*[:is]*\s*([A-Z0-9]{6})/i);
    if (verifyMatch) {
        if (!quietMode) console.log(`[线程 ${threadId}] 通过verification找到验证码: ${verifyMatch[1]}`);
        return verifyMatch[1];
    }

    // 方法4: 在HTML标签中查找
    const htmlMatch = content.match(/>\s*([A-Z0-9]{6})\s*</g);
    if (htmlMatch && htmlMatch.length > 0) {
        const code = htmlMatch[0].replace(/[><\s]/g, '');
        if (!commonWords.includes(code) && /[0-9]/.test(code)) {
            if (!quietMode) console.log(`[线程 ${threadId}] 从HTML标签找到验证码: ${code}`);
            return code;
        }
    }

    console.error(`[线程 ${threadId}] 无法提取验证码，邮件内容: ${content.substring(0, 500)}`);
    throw new Error(`无法从邮件中提取验证码。邮件主题: ${subject}`);
}

// 生成随机全名
function generateRandomName() {
    const firstNames = ['John', 'Jane', 'Michael', 'Sarah', 'David', 'Emily', 'Robert', 'Lisa'];
    const lastNames = ['Smith', 'Johnson', 'Williams', 'Brown', 'Jones', 'Garcia', 'Miller', 'Davis'];

    const firstName = firstNames[Math.floor(Math.random() * firstNames.length)];
    const lastName = lastNames[Math.floor(Math.random() * lastNames.length)];

    return `${firstName} ${lastName}`;
}

// 全局统计
const stats = {
    total: 0,
    success: 0,
    failed: 0,
    startTime: Date.now()
};

// 打印统计信息
function printStats() {
    if (quietMode) return;
    const duration = ((Date.now() - stats.startTime) / 1000 / 60).toFixed(2);
    console.log('\n=== 运行统计 ===');
    console.log(`运行时间: ${duration} 分钟`);
    console.log(`总尝试数: ${stats.total}`);
    console.log(`成功数量: ${stats.success}`);
    console.log(`失败数量: ${stats.failed}`);
    console.log(`成功率: ${stats.total > 0 ? ((stats.success / stats.total) * 100).toFixed(2) : 0}%`);
    console.log('================\n');
}

// 定期打印统计
setInterval(printStats, 60000);

async function runTask(threadId, config) {
    let browser;
    quietMode = config.quiet;
    if (!quietMode) console.log(`[线程 ${threadId}] 启动任务`);
    stats.total++;

    try {
        // 获取临时邮箱
        const email = await getTemporaryEmail(threadId);

        // 启动浏览器
        const launchArgs = [
            '--no-sandbox',
            '--disable-setuid-sandbox',
            '--disable-dev-shm-usage',
            '--disable-gpu',
            '--incognito'
        ];
        if (config.proxy) {
            launchArgs.push(`--proxy-server=${config.proxy}`);
        }
        
        if (!quietMode) console.log(`[线程 ${threadId}] 正在启动浏览器...`);
        browser = await puppeteer.launch({
            headless: config.headless ? 'new' : false,
            args: launchArgs
        });

        // 获取默认页面（避免打开两个窗口）
        const pages = await browser.pages();
        const page = pages.length > 0 ? pages[0] : await browser.newPage();

        // 设置 User-Agent
        await page.setUserAgent('Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0');

        // 监听请求以捕获 authorization 和 cookie
        let authData = {
            authorization: null,
            cookies: null,
            configId: null,
            csesidx: null
        };

        page.on('request', (request) => {
            const headers = request.headers();
            if (headers['authorization']) {
                authData.authorization = headers['authorization'];
            }
        });

        // 监听所有响应以提取 configId 和 csesidx
        page.on('response', async (response) => {
            try {
                const url = response.url();
                // 提取 configId: /cid/xxx
                const cidMatch = url.match(/\/cid\/([a-f0-9-]+)/i);
                if (cidMatch && !authData.configId) {
                    authData.configId = cidMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从响应提取 configId: ${authData.configId}`);
                }
                // 提取 csesidx: ?csesidx=xxx 或 &csesidx=xxx
                const csesidxMatch = url.match(/[?&]csesidx=(\d+)/);
                if (csesidxMatch && !authData.csesidx) {
                    authData.csesidx = csesidxMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从响应提取 csesidx: ${authData.csesidx}`);
                }
            } catch (e) {
                // 忽略错误
            }
        });

        // 同时监听 URL 变化
        page.on('framenavigated', async (frame) => {
            if (frame === page.mainFrame()) {
                const url = frame.url();
                const cidMatch = url.match(/\/cid\/([a-f0-9-]+)/i);
                if (cidMatch && !authData.configId) {
                    authData.configId = cidMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从URL提取 configId: ${authData.configId}`);
                }
                const csesidxMatch = url.match(/[?&]csesidx=(\d+)/);
                if (csesidxMatch && !authData.csesidx) {
                    authData.csesidx = csesidxMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从URL提取 csesidx: ${authData.csesidx}`);
                }
            }
        });
        await page.goto('https://business.gemini.google', {
            waitUntil: 'networkidle2',
            timeout: 60000
        });
        // 等待输入框出现
        await page.waitForSelector('input', { timeout: 30000 });
        await new Promise(resolve => setTimeout(resolve, 2000));

        // 先点击输入框聚焦
        await page.evaluate(() => {
            const inputs = document.querySelectorAll('input');
            if (inputs.length > 0) {
                inputs[0].click();
                inputs[0].focus();
            }
        });

        await new Promise(resolve => setTimeout(resolve, 1000));

        // 使用 type 方法模拟真实键盘输入
        await page.type('input', email, { delay: 30 });
        if (!quietMode) console.log(`[线程 ${threadId}] 已填写邮箱:`, email);

        // 等待一下
        await new Promise(resolve => setTimeout(resolve, 2000));

        // 验证输入框的值
        const actualValue = await page.evaluate(() => {
            const inputs = document.querySelectorAll('input');
            return inputs.length > 0 ? inputs[0].value : '';
        });

        // 触发 blur 事件以确保验证
        await page.evaluate(() => {
            const inputs = document.querySelectorAll('input');
            if (inputs.length > 0) {
                inputs[0].blur();
            }
        });

        await new Promise(resolve => setTimeout(resolve, 1000));

        // 查找并点击按钮 (带重试)
        let emailSubmitted = false;
        for (let i = 0; i < 5; i++) {
            const clicked = await page.evaluate(() => {
                const targets = ['继续', 'Next', '邮箱', 'Next', 'Continue'];
                const elements = [
                    ...document.querySelectorAll('button'),
                    ...document.querySelectorAll('input[type="submit"]'),
                    ...document.querySelectorAll('div[role="button"]'),
                    ...document.querySelectorAll('span[role="button"]')
                ];

                for (const element of elements) {
                    // 检查可见性
                    const style = window.getComputedStyle(element);
                    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
                    if (element.disabled) continue;

                    const text = element.textContent.trim();
                    if (targets.some(t => text.includes(t))) {
                        element.click();
                        return true;
                    }
                }

                // 备用：查找主要按钮
                const primaryBtn = document.querySelector('button[color="primary"], button.primary');
                if (primaryBtn && !primaryBtn.disabled) {
                    primaryBtn.click();
                    return true;
                }

                return false;
            });

            if (clicked) {
                emailSubmitted = true;
                break;
            }
            await new Promise(resolve => setTimeout(resolve, 1000));
        }

        if (!emailSubmitted) {
            throw new Error('找不到提交按钮');
        }
        await new Promise(resolve => setTimeout(resolve, 5000));

        // 检查邮箱提交结果：是否发送成功、是否有错误
        const emailSubmitResult = await page.evaluate(() => {
            const pageText = document.body.textContent;
            const pageHtml = document.body.innerHTML.toLowerCase();
            
            // 检查常见错误信息
            const errorPatterns = [
                // 邮箱相关错误
                { pattern: /invalid.*email|email.*invalid/i, error: '邮箱格式无效' },
                { pattern: /email.*already.*use|already.*registered/i, error: '邮箱已被注册' },
                { pattern: /email.*not.*allowed|not.*accept.*email/i, error: '邮箱域名不被接受' },
                { pattern: /temporary.*email|disposable.*email/i, error: '临时邮箱不被接受' },
                // 频率限制
                { pattern: /too.*many.*request|rate.*limit|try.*again.*later/i, error: '请求过于频繁' },
                { pattern: /slow.*down|wait.*before/i, error: '操作过快，请稍后重试' },
                // 通用错误
                { pattern: /something.*went.*wrong|error.*occurred/i, error: '发生未知错误' },
                { pattern: /couldn.*send|failed.*send|unable.*send/i, error: '验证码发送失败' },
                // 中文错误
                { pattern: /无效.*邮箱|邮箱.*无效/i, error: '邮箱格式无效' },
                { pattern: /已.*注册|已.*使用/i, error: '邮箱已被注册' },
                { pattern: /请.*稍后|频繁/i, error: '请求过于频繁' },
                { pattern: /发送失败|无法发送/i, error: '验证码发送失败' },
            ];
            
            for (const { pattern, error } of errorPatterns) {
                if (pattern.test(pageText)) {
                    return { success: false, error: error, needsVerification: false };
                }
            }
            
            // 检查是否有错误提示元素（红色文字、错误图标等）
            const errorElements = document.querySelectorAll('[class*="error"], [class*="Error"], [role="alert"], .error-message');
            for (const el of errorElements) {
                const text = el.textContent.trim();
                if (text && text.length > 0 && text.length < 200) {
                    return { success: false, error: `页面错误: ${text}`, needsVerification: false };
                }
            }
            
            // 检查是否成功进入验证码页面
            if (pageText.includes('验证') || pageText.includes('Verify') || pageText.includes('verification') || 
                pageText.includes('code') || pageText.includes('验证码') || pageText.includes('sent')) {
                return { success: true, error: null, needsVerification: true };
            }
            
            // 检查是否已经到了全名输入页面（跳过验证码）
            if (pageText.includes('姓氏') || pageText.includes('名字') || pageText.includes('name') || 
                pageText.includes('Full name') || pageText.includes('全名')) {
                return { success: true, error: null, needsVerification: false };
            }
            
            // 默认认为成功，需要验证
            return { success: true, error: null, needsVerification: true };
        });

        // 处理邮箱提交结果
        if (!emailSubmitResult.success) {
            // 频率限制错误，等待后重试
            if (emailSubmitResult.error.includes('频繁') || emailSubmitResult.error.includes('稍后')) {
                if (!quietMode) console.log(`[线程 ${threadId}] ⏳ 请求过于频繁，等待 60 秒后重试...`);
                await new Promise(resolve => setTimeout(resolve, 60000));
                throw new Error(`邮箱提交失败: ${emailSubmitResult.error}，已等待，请重试`);
            }
            console.error(`[线程 ${threadId}] ✗ 邮箱提交失败: ${emailSubmitResult.error}`);
            throw new Error(`邮箱提交失败: ${emailSubmitResult.error}`);
        }

        if (!quietMode) console.log(`[线程 ${threadId}] ✓ 邮箱提交成功，需要验证码: ${emailSubmitResult.needsVerification}`);

        const needsVerification = emailSubmitResult.needsVerification;

        let verificationCode = null;

        if (needsVerification) {
            if (!quietMode) console.log(`[线程 ${threadId}] 页面需要验证码，开始获取邮件...`);

            // 获取验证码邮件（支持重发）
            const maxResendAttempts = 3;
            let emailData = null;
            
            for (let resendAttempt = 0; resendAttempt < maxResendAttempts; resendAttempt++) {
                // 尝试在 15 秒内获取验证码（5 次尝试，每次 3 秒）
                const quickRetries = 5;
                let gotEmail = false;
                
                for (let i = 0; i < quickRetries; i++) {
                    try {
                        const response = await axios.get(`https://mail.chatgpt.org.uk/api/emails?email=${email}`, {
                            headers: {
                                'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0',
                                'referer': 'https://mail.chatgpt.org.uk'
                            },
                            timeout: 5000
                        });

                        if (response.data.success && response.data.data.emails.length > 0) {
                            emailData = response.data.data.emails[0];
                            gotEmail = true;
                            break;
                        }
                    } catch (error) {
                        if (!quietMode) console.log(`[线程 ${threadId}] 获取邮件失败: ${error.message}`);
                    }
                    
                    if (!quietMode) console.log(`[线程 ${threadId}] 等待验证码邮件... (${(i + 1) * 3}s/${quickRetries * 3}s)`);
                    await new Promise(resolve => setTimeout(resolve, 3000));
                }
                
                if (gotEmail) {
                    if (!quietMode) console.log(`[线程 ${threadId}] ✓ 成功获取验证码邮件`);
                    break;
                }
                
                // 15 秒内未收到邮件，尝试点击重发
                if (resendAttempt < maxResendAttempts - 1) {
                    if (!quietMode) console.log(`[线程 ${threadId}] ⚠️ 15秒内未收到验证码，尝试点击重发... (${resendAttempt + 1}/${maxResendAttempts - 1})`);
                    
                    const resendClicked = await page.evaluate(() => {
                        const resendTexts = ['重新发送', 'Resend', 'resend', '重发', 'Send again', 'send again', '再次发送' , '重新发送验证码', '发送'];
                        const elements = [
                            ...document.querySelectorAll('a'),
                            ...document.querySelectorAll('button'),
                            ...document.querySelectorAll('span'),
                            ...document.querySelectorAll('div[role="button"]'),
                            ...document.querySelectorAll('[class*="resend"]'),
                            ...document.querySelectorAll('[class*="Resend"]')
                        ];
                        
                        for (const element of elements) {
                            const text = element.textContent.trim();
                            const style = window.getComputedStyle(element);
                            if (style.display === 'none' || style.visibility === 'hidden') continue;
                            
                            if (resendTexts.some(t => text.toLowerCase().includes(t.toLowerCase()))) {
                                element.click();
                                return true;
                            }
                        }
                        return false;
                    });
                    
                    if (resendClicked) {
                        if (!quietMode) console.log(`[线程 ${threadId}] ✓ 已点击重发验证码按钮`);
                        await new Promise(resolve => setTimeout(resolve, 3000));
                    } else {
                        if (!quietMode) console.log(`[线程 ${threadId}] ⚠️ 未找到重发按钮`);
                    }
                }
            }
            
            if (!emailData) {
                throw new Error('多次尝试后仍无法获取验证码邮件');
            }

            try {
                verificationCode = extractVerificationCode(emailData, threadId);
                if (!quietMode) console.log(`[线程 ${threadId}] ✓ 成功提取验证码: ${verificationCode}`);
            } catch (err) {
                console.error(`[线程 ${threadId}] ✗ 验证码提取失败:`, err.message);
                throw err;
            }


            // 等待验证码输入框并确保页面稳定
            await page.waitForSelector('input', { timeout: 30000 });
            await new Promise(resolve => setTimeout(resolve, 2000));

            // 清空可能的旧值并聚焦
            await page.evaluate(() => {
                const inputs = document.querySelectorAll('input');
                if (inputs.length > 0) {
                    inputs[0].value = '';
                    inputs[0].click();
                    inputs[0].focus();
                }
            });

            await new Promise(resolve => setTimeout(resolve, 500));

            // 使用 type 方法输入验证码
            await page.type('input', verificationCode, { delay: 30 });
            if (!quietMode) console.log(`[线程 ${threadId}] 已填写验证码`);

            await new Promise(resolve => setTimeout(resolve, 2000));

            // 触发 blur
            await page.evaluate(() => {
                const inputs = document.querySelectorAll('input');
                if (inputs.length > 0) {
                    inputs[0].blur();
                }
            });

            await new Promise(resolve => setTimeout(resolve, 1000));
            let verifySubmitted = false;
            for (let i = 0; i < 5; i++) {
                const verifyClicked = await page.evaluate(() => {
                    const targets = ['验证', 'Verify', '继续', 'Next', 'Continue'];
                    const elements = [
                        ...document.querySelectorAll('button'),
                        ...document.querySelectorAll('input[type="submit"]'),
                        ...document.querySelectorAll('div[role="button"]')
                    ];

                    for (const element of elements) {
                        const style = window.getComputedStyle(element);
                        if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
                        if (element.disabled) continue;

                        const text = element.textContent.trim();
                        if (targets.some(t => text.includes(t))) {
                            element.click();
                            return true;
                        }
                    }
                    return false;
                });

                if (verifyClicked) {
                    verifySubmitted = true;
                    break;
                } else {
                    if (!quietMode) console.log(`[线程 ${threadId}] 尝试 ${i + 1}/5: 未找到验证提交按钮，等待重试...`);
                }
                await new Promise(resolve => setTimeout(resolve, 1500));
            }

            // 等待重定向
            if (!quietMode) console.log(`[线程 ${threadId}] 等待重定向...`);
            await new Promise(resolve => setTimeout(resolve, 5000));

            // 检查验证码提交结果（处理页面导航导致的上下文销毁）
            let verifyResult = { success: true, error: null };
            try {
                verifyResult = await page.evaluate(() => {
                    const pageText = document.body.textContent;
                    
                    // 检查验证码错误
                    const errorPatterns = [
                        { pattern: /invalid.*code|code.*invalid|wrong.*code|incorrect.*code/i, error: '验证码错误' },
                        { pattern: /expired|过期/i, error: '验证码已过期' },
                        { pattern: /too.*many.*attempt|多次.*尝试/i, error: '尝试次数过多' },
                        // 中文
                        { pattern: /验证码.*错误|错误.*验证码|无效.*验证码/i, error: '验证码错误' },
                    ];
                    
                    for (const { pattern, error } of errorPatterns) {
                        if (pattern.test(pageText)) {
                            return { success: false, error: error };
                        }
                    }
                    
                    // 检查是否成功进入下一步（全名输入页面）
                    if (pageText.includes('姓氏') || pageText.includes('名字') || pageText.includes('name') || 
                        pageText.includes('Full name') || pageText.includes('全名')) {
                        return { success: true, error: null };
                    }
                    
                    // 检查是否还在验证码页面（可能验证失败）
                    const inputs = document.querySelectorAll('input');
                    if (inputs.length > 0) {
                        const inputValue = inputs[0].value;
                        // 如果输入框还有验证码，可能是验证失败
                        if (inputValue && inputValue.length === 6) {
                            return { success: false, error: '验证码可能无效，页面未跳转' };
                        }
                    }
                    
                    // 默认认为成功
                    return { success: true, error: null };
                });
            } catch (err) {
                // 页面导航导致上下文销毁，说明验证成功并跳转了
                if (err.message.includes('Execution context was destroyed') || 
                    err.message.includes('navigation')) {
                    if (!quietMode) console.log(`[线程 ${threadId}] ✓ 页面已跳转，验证码验证成功`);
                    verifyResult = { success: true, error: null };
                } else {
                    throw err;
                }
            }

            if (!verifyResult.success) {
                console.error(`[线程 ${threadId}] ✗ 验证码验证失败: ${verifyResult.error}`);
                throw new Error(`验证码验证失败: ${verifyResult.error}`);
            }

            if (!quietMode) console.log(`[线程 ${threadId}] ✓ 验证码验证成功`);
        } else {
            if (!quietMode) console.log(`[线程 ${threadId}] 页面已跳过验证码步骤，直接进入下一步`);
        }

        // 生成随机全名
        const fullName = generateRandomName();
        if (!quietMode) console.log(`[线程 ${threadId}] 生成的全名:`, fullName);

        // 等待输入框并确保页面稳定
        await page.waitForSelector('input', { timeout: 30000 });
        await new Promise(resolve => setTimeout(resolve, 2000));

        // 清空可能的旧值并聚焦
        await page.evaluate(() => {
            const inputs = document.querySelectorAll('input');
            if (inputs.length > 0) {
                inputs[0].value = '';
                inputs[0].click();
                inputs[0].focus();
            }
        });

        await new Promise(resolve => setTimeout(resolve, 500));

        // 使用 type 方法输入全名
        await page.type('input', fullName, { delay: 30 });
        if (!quietMode) console.log(`[线程 ${threadId}] 已填写全名`);

        await new Promise(resolve => setTimeout(resolve, 2000));

        // 触发 blur
        await page.evaluate(() => {
            const inputs = document.querySelectorAll('input');
            if (inputs.length > 0) {
                inputs[0].blur();
            }
        });

        await new Promise(resolve => setTimeout(resolve, 1000));

        // 确认提交 (带重试)
        if (!quietMode) console.log(`[线程 ${threadId}] 准备提交全名...`);
        let confirmSubmitted = false;
        for (let i = 0; i < 5; i++) {
            const confirmClicked = await page.evaluate(() => {
                const targets = ['同意', 'Confirm', '继续', 'Next', 'Continue'];
                const elements = [
                    ...document.querySelectorAll('button'),
                    ...document.querySelectorAll('input[type="submit"]'),
                    ...document.querySelectorAll('div[role="button"]')
                ];

                for (const element of elements) {
                    const style = window.getComputedStyle(element);
                    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
                    if (element.disabled) continue;

                    const text = element.textContent.trim();
                    if (targets.some(t => text.includes(t))) {
                        element.click();
                        return true;
                    }
                }

                // 备用: 点击第一个可见的按钮
                for (const element of elements) {
                    if (element.offsetParent !== null && !element.disabled) {
                        element.click();
                        return true;
                    }
                }
                return false;
            });

            if (confirmClicked) {
                confirmSubmitted = true;
                break;
            } else {
                if (!quietMode) console.log(`[线程 ${threadId}] 尝试 ${i + 1}/5: 未找到确认按钮，等待重试...`);
            }
            await new Promise(resolve => setTimeout(resolve, 1500));
        }

        if (!confirmSubmitted) {
        }
        // 循环检查 authorization，如果没获取到就继续尝试点击按钮
        await new Promise(resolve => setTimeout(resolve, 2000));

        let retries = 0;
        while (!authData.authorization && retries < 10) {

            // 尝试点击可能出现的"继续"或"同意"按钮
            let clickedNext = false;
            try {
                clickedNext = await page.evaluate(() => {
                    const buttons = document.querySelectorAll('button');
                    for (const button of buttons) {
                        const text = button.textContent;
                        if (text.includes('同意') || text.includes('Confirm') || text.includes('继续') || text.includes('Next') || text.includes('I agree')) {
                            if (button.offsetParent !== null && !button.disabled) {
                                button.click();
                                return true;
                            }
                        }
                    }
                    return false;
                });
            } catch (err) {
                // 忽略页面导航导致的上下文销毁错误
                if (!err.message.includes('Execution context was destroyed')) {
                    console.error(`[线程 ${threadId}] 检查按钮时出错:`, err.message);
                }
            }

            if (clickedNext) {
                if (!quietMode) console.log(`[线程 ${threadId}] 点击了额外的继续按钮`);
            }

            await new Promise(resolve => setTimeout(resolve, 3000));
            retries++;
        }

        if (!authData.authorization) {
            throw new Error('未能获取 Authorization，注册可能未完成');
        }

        // 获取最终的 cookies
        const cookies = await page.cookies();
        authData.cookies = cookies;

        // 保存数据
        const dataDir = await ensureDataDir(config.dataDir);
        const outputFile = path.join(dataDir, `${email}.json`);

        // 等待页面稳定后多次尝试提取 configId 和 csesidx
        for (let attempt = 0; attempt < 5 && (!authData.configId || !authData.csesidx); attempt++) {
            await new Promise(resolve => setTimeout(resolve, 2000));
            const currentUrl = page.url();
            if (!quietMode) console.log(`[线程 ${threadId}] 当前URL: ${currentUrl}`);
            
            if (!authData.configId) {
                const cidMatch = currentUrl.match(/\/cid\/([a-f0-9-]+)/i);
                if (cidMatch) {
                    authData.configId = cidMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从最终URL提取 configId: ${authData.configId}`);
                }
            }
            if (!authData.csesidx) {
                const csesidxMatch = currentUrl.match(/[?&]csesidx=(\d+)/);
                if (csesidxMatch) {
                    authData.csesidx = csesidxMatch[1];
                    if (!quietMode) console.log(`[线程 ${threadId}] 从最终URL提取 csesidx: ${authData.csesidx}`);
                }
            }
        }

        // 如果还是没有，警告但继续保存
        if (!authData.configId || !authData.csesidx) {
            if (!quietMode) console.log(`[线程 ${threadId}] ⚠️ 未能提取完整信息: configId=${authData.configId}, csesidx=${authData.csesidx}`);
        } else {
            if (!quietMode) console.log(`[线程 ${threadId}] ✓ 提取成功: configId=${authData.configId}, csesidx=${authData.csesidx}`);
        }

        await fs.writeFile(outputFile, JSON.stringify({
            email: email,
            fullName: fullName,
            authorization: authData.authorization,
            cookies: authData.cookies,
            configId: authData.configId,
            csesidx: authData.csesidx,
            timestamp: new Date().toISOString()
        }, null, 2));
        stats.success++;
        if (!quietMode) console.log(`[线程 ${threadId}] ✓ 账号保存成功: ${email}`);
        // 输出结构化结果供 Go 程序解析
        console.log(`@@REGISTER_RESULT@@${JSON.stringify({ success: true, email: email, error: null })}@@END@@`);

    } catch (error) {
        console.error(`[线程 ${threadId}] 发生错误:`, error.message);
        stats.failed++;
        // 输出结构化结果供 Go 程序解析
        const needWait = error.message.includes('频繁') || error.message.includes('稍后') || error.message.includes('rate');
        console.log(`@@REGISTER_RESULT@@${JSON.stringify({ success: false, email: null, error: error.message, needWait: needWait })}@@END@@`);
    } finally {
        if (browser) {
            // 等待 5 秒后关闭浏览器
            await new Promise(resolve => setTimeout(resolve, 5000));
            await browser.close();
        }
    }
}

async function worker(threadId, config) {
    if (!quietMode) console.log(`[线程 ${threadId}] 启动 Worker`);
    while (true) {
        try {
            await runTask(threadId, config);
        } catch (error) {
            console.error(`[线程 ${threadId}] 任务执行异常:`, error);
        }

        if (!config.continuous) {
            if (!quietMode) console.log(`[线程 ${threadId}] 单次运行完成，退出 Worker`);
            break;
        }

        // 任务之间添加短暂延迟
        if (!quietMode) console.log(`[线程 ${threadId}] 准备开始下一个任务...`);
        await new Promise(resolve => setTimeout(resolve, 2000));
    }
}

async function main() {
    const config = await loadConfig();
    // 默认配置（命令行模式默认单次运行）
    if (config.continuous === undefined) {
        config.continuous = process.argv.length <= 2; // 无命令行参数时持续运行
    }

    quietMode = config.quiet;
    if (!quietMode) {
        console.log(`配置: Headless=${config.headless}, Threads=${config.threads}, Continuous=${config.continuous}, DataDir=${config.dataDir || 'default'}`);
        console.log(config.continuous ? '开始持续运行模式...' : '开始单次运行模式...');
    }

    const workers = [];
    for (let i = 0; i < config.threads; i++) {
        workers.push(worker(i + 1, config));
    }

    await Promise.all(workers);
    printStats();
    if (!quietMode) console.log('所有任务完成');
    // 输出最终统计供 Go 程序解析
    console.log(`@@REGISTER_STATS@@${JSON.stringify(stats)}@@END@@`);
    
    // 非持续模式下强制退出
    if (!config.continuous) {
        process.exit(0);
    }
}

main();

