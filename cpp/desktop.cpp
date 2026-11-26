#include "core.h"
#include <QtWidgets>
#include <QElapsedTimer>
#include <QRegularExpression>
#include <QStandardPaths>
#include <QUrl>
#include <algorithm>
#include <cmath>
#include <future>
#include <memory>
#include <vector>
#ifdef Q_OS_WIN
#define NOMINMAX
#include <windows.h>
#endif

namespace {

constexpr unsigned kConnections = 8;
constexpr int kCardHeight = 84;

enum class State { Queued, Running, Paused, Done, Failed };

struct Theme {
    QColor bg, surface, surfaceAlt, border, text, muted, accent, accentHover;
    QColor success, warn, danger, hover, select;
    static Theme light() {
        return {QColor("#E4E9F3"), QColor("#FFFFFF"), QColor("#F5F8FD"), QColor("#DCE2EE"),
                QColor("#101828"), QColor("#667085"), QColor("#2F6BFF"), QColor("#1F55E0"),
                QColor("#12A150"), QColor("#C77700"), QColor("#D92D20"),
                QColor("#F2F5FB"), QColor("#E7EEFF")};
    }
    static Theme dark() {
        return {QColor("#0B101B"), QColor("#151C2A"), QColor("#1B2233"), QColor("#28324A"),
                QColor("#E8EDF7"), QColor("#93A0B8"), QColor("#5B8CFF"), QColor("#7BA5FF"),
                QColor("#35C46B"), QColor("#F0A63C"), QColor("#FF6B5E"),
                QColor("#1E2739"), QColor("#23314D")};
    }
};

QString bytes(qint64 n) { return n > 0 ? QLocale().formattedDataSize(n) : QStringLiteral("—"); }
QString speed(double perSecond) {
    return perSecond >= 1 ? bytes(qint64(perSecond)) + QStringLiteral("/s") : QStringLiteral("—");
}
QString badge(const QString& name) {
    auto ext = QFileInfo(name).suffix().toUpper();
    return ext.isEmpty() ? QStringLiteral("FILE") : ext.left(4);
}
QColor badgeColor(const QString& name, const Theme& t) {
    const auto ext = QFileInfo(name).suffix().toLower();
    if (ext == "exe" || ext == "msi") return t.accent;
    if (ext == "zip" || ext == "rar" || ext == "7z") return t.warn;
    if (ext == "mp4" || ext == "mkv" || ext == "mp3") return QColor("#8B5CF6");
    if (ext == "jpg" || ext == "png" || ext == "gif") return t.success;
    if (ext == "pdf" || ext == "doc" || ext == "docx") return t.danger;
    return t.muted;
}
QString sanitise(QString name) {
    name.replace(QRegularExpression(QStringLiteral("[<>:\"/\\\\|?*\\x00-\\x1f]")), QStringLiteral("_"));
    name = name.trimmed();
    if (name.isEmpty() || name == "." || name == "..") return QStringLiteral("download");
    return name;
}
QString defaultFolder() { return QStandardPaths::writableLocation(QStandardPaths::DownloadLocation); }

QString themeStyleSheet(const Theme& t) {
    return QStringLiteral(R"(
* { font-family: "Segoe UI", "Inter", sans-serif; }
QWidget#root { background: %1; }
QFrame#header { background: %2; border-bottom: 1px solid %3; }
QFrame#strip { background: qlineargradient(x1:0, y1:0, x2:1, y2:0, stop:0 %6, stop:1 #7C5CFF); border: none; }
QFrame#panel, QFrame#detail { background: %2; border: 1px solid %3; border-radius: 14px; }
QLabel#wordmark { color: %4; font-size: 17px; font-weight: 700; letter-spacing: 0.5px; }
QLabel#tagline { color: %5; font-size: 12px; }
QLabel#logo {
    background: %6; color: #FFFFFF; border-radius: 10px;
    font-size: 14px; font-weight: 700; letter-spacing: 0.5px;
}
QLabel#sectionTitle { color: %5; font-size: 11px; font-weight: 600; letter-spacing: 1px; }
QLabel#detailName { color: %4; font-size: 15px; font-weight: 600; }
QLabel#detailHost { color: %5; font-size: 12px; }
QLabel#key { color: %5; font-size: 12px; }
QLabel#value { color: %4; font-size: 12px; }
QLabel#empty { color: %5; font-size: 14px; }
QLabel#emptyTitle { color: %4; font-size: 18px; font-weight: 600; }
QLabel#footer { color: %5; font-size: 12px; }
QLineEdit#search {
    background: %8; border: 1px solid %3; border-radius: 9px; padding: 7px 12px;
    color: %4; font-size: 13px; selection-background-color: %6;
}
QLineEdit#search:focus { border: 1px solid %6; background: %2; }
QPushButton {
    background: transparent; color: %4; border: 1px solid %3; border-radius: 9px;
    padding: 7px 14px; font-size: 13px; font-weight: 500;
}
QPushButton:hover { background: %7; border-color: %3; }
QPushButton:pressed { background: %9; }
QPushButton#primary {
    background: %6; color: #FFFFFF; border: 1px solid %6; font-weight: 600; padding: 8px 18px;
}
QPushButton#primary:hover { background: %10; border-color: %10; }
QPushButton#ghost { padding: 7px 10px; }
QListView#list { background: transparent; border: none; outline: none; }
QListView#side { background: transparent; border: none; outline: none; }
QScrollBar:vertical { background: transparent; width: 10px; margin: 4px 2px 4px 0; }
QScrollBar::handle:vertical { background: %3; border-radius: 4px; min-height: 30px; }
QScrollBar::handle:vertical:hover { background: %5; }
QScrollBar::add-line:vertical, QScrollBar::sub-line:vertical { height: 0; }
QScrollBar::add-page:vertical, QScrollBar::sub-page:vertical { background: none; }
QFrame#footer { background: %2; border-top: 1px solid %3; }
QFrame#divider { background: %3; max-width: 1px; border: none; }
QDialog { background: %2; }
QSpinBox, QLineEdit#field {
    background: %8; border: 1px solid %3; border-radius: 9px; padding: 7px 10px;
    color: %4; font-size: 13px; selection-background-color: %6;
}
QSpinBox:focus, QLineEdit#field:focus { border: 1px solid %6; background: %2; }
QToolTip { background: %2; color: %4; border: 1px solid %3; padding: 4px 8px; }
)")
        .arg(t.bg.name(), t.surface.name(), t.border.name(), t.text.name(), t.muted.name(),
             t.accent.name(), t.hover.name(), t.surfaceAlt.name(), t.select.name(), t.accentHover.name());
}

}  // namespace

// ─────────────────────────────── task state ────────────────────────────────

struct Task {
    QString url, path, host, name;
    State state = State::Queued;
    QString error;
    odm::Progress progress;
    std::future<bool> worker;
    double rate = 0;
    qint64 lastBytes = 0;
    QElapsedTimer sample;
    bool pausedByUser = false;
};

class TaskModel : public QAbstractListModel {
public:
    enum Role { TaskRole = Qt::UserRole + 1 };
    using QAbstractListModel::QAbstractListModel;
    int rowCount(const QModelIndex& parent = {}) const override {
        return parent.isValid() ? 0 : int(tasks.size());
    }
    QVariant data(const QModelIndex& index, int role) const override {
        if (!index.isValid() || index.row() >= rowCount()) return {};
        const auto& t = *tasks[size_t(index.row())];
        if (role == Qt::DisplayRole) return t.name;
        if (role == Qt::ToolTipRole) return t.path;
        if (role == TaskRole) return QVariant::fromValue(static_cast<void*>(const_cast<Task*>(&t)));
        return {};
    }
    Task* at(int row) const { return row >= 0 && row < rowCount() ? tasks[size_t(row)].get() : nullptr; }
    int append(std::unique_ptr<Task> task) {
        const int row = rowCount();
        beginInsertRows({}, row, row);
        tasks.push_back(std::move(task));
        endInsertRows();
        return row;
    }
    void erase(int row) {
        if (row < 0 || row >= rowCount()) return;
        beginRemoveRows({}, row, row);
        tasks.erase(tasks.begin() + row);
        endRemoveRows();
    }
    void notify(int row) {
        if (row >= 0 && row < rowCount()) emit dataChanged(index(row), index(row));
    }
    std::vector<std::unique_ptr<Task>> tasks;
};

class FilterProxy : public QSortFilterProxyModel {
public:
    FilterProxy(TaskModel* source, QObject* parent = nullptr) : QSortFilterProxyModel(parent), m_source(source) {
        setSourceModel(source);
        setDynamicSortFilter(true);
    }
    int filter = 0;
    QString query;
    void applyFilter() { invalidateFilter(); }
    bool accepts(int state) const {
        switch (filter) {
            case 1: return state == int(State::Running);
            case 2: return state == int(State::Queued);
            case 3: return state == int(State::Done);
            case 4: return state == int(State::Paused) || state == int(State::Failed);
            default: return true;
        }
    }
protected:
    bool filterAcceptsRow(int row, const QModelIndex&) const override {
        const auto* task = m_source->at(row);
        if (!task || !accepts(int(task->state))) return false;
        return query.isEmpty() || task->name.contains(query, Qt::CaseInsensitive) ||
               task->host.contains(query, Qt::CaseInsensitive);
    }
private:
    TaskModel* m_source;
};

// ─────────────────────────────── list delegate ─────────────────────────────

class TaskDelegate : public QStyledItemDelegate {
public:
    TaskDelegate(const Theme* theme, TaskModel* model, QObject* parent = nullptr)
        : QStyledItemDelegate(parent), m_theme(theme), m_model(model) {}

    QSize sizeHint(const QStyleOptionViewItem&, const QModelIndex&) const override {
        return {0, kCardHeight};
    }

    void paint(QPainter* p, const QStyleOptionViewItem& option, const QModelIndex& proxyIndex) const override {
        const auto* task = static_cast<const Task*>(proxyIndex.data(TaskModel::TaskRole).value<void*>());
        if (!task) return;
        const auto& t = *m_theme;
        p->save();
        p->setRenderHints(QPainter::Antialiasing | QPainter::TextAntialiasing);
        const QRect card = option.rect.adjusted(10, 5, -10, -5);
        const bool selected = option.state & QStyle::State_Selected;
        const bool hover = option.state & QStyle::State_MouseOver;
        QColor fill = t.surface;
        if (selected) fill = t.select;
        else if (hover) fill = t.surfaceAlt;
        if (!selected) {
            QColor shadow(16, 24, 40);
            shadow.setAlpha(t.bg.lightness() > 128 ? 10 : 40);
            p->setPen(Qt::NoPen);
            p->setBrush(shadow);
            p->drawRoundedRect(QRectF(card).translated(0, 2), 12, 12);
        }
        p->setPen(Qt::NoPen);
        p->setBrush(fill);
        p->drawRoundedRect(card, 12, 12);
        p->setBrush(Qt::NoBrush);
        p->setPen(QPen(selected ? t.accent : t.border, selected ? 1.6 : 1.0));
        p->drawRoundedRect(QRectF(card).adjusted(0.5, 0.5, -0.5, -0.5), 12, 12);

        // extension badge
        const QRect icon(card.left() + 16, card.top() + 15, 40, 40);
        p->setPen(Qt::NoPen);
        QColor accent = badgeColor(task->name, t);
        QColor soft = accent;
        soft.setAlpha(t.bg.lightness() > 128 ? 28 : 46);
        p->setBrush(soft);
        p->drawRoundedRect(icon, 10, 10);
        QFont mono = option.font;
        mono.setPointSizeF(std::max(7.5, option.font.pointSizeF() - 2.5));
        mono.setBold(true);
        p->setFont(mono);
        p->setPen(accent);
        p->drawText(icon, Qt::AlignCenter, badge(task->name));

        const QRect right = card.adjusted(card.width() - 210, 0, -16, 0);

        // status pill
        QString label;
        QColor pill;
        switch (task->state) {
            case State::Running: label = task->progress.total ? QStringLiteral("Downloading") : QStringLiteral("Connecting"); pill = t.accent; break;
            case State::Paused: label = QStringLiteral("Paused"); pill = t.warn; break;
            case State::Done: label = QStringLiteral("Complete"); pill = t.success; break;
            case State::Failed: label = QStringLiteral("Failed"); pill = t.danger; break;
            default: label = QStringLiteral("Queued"); pill = t.muted; break;
        }
        QFont pillFont = option.font;
        pillFont.setPointSizeF(std::max(8.0, option.font.pointSizeF() - 1.5));
        const QFontMetrics fmSmall(pillFont);
        const int pillWidth = fmSmall.horizontalAdvance(label) + 22;
        const QRect pillRect(right.right() - pillWidth, card.top() + 15, pillWidth, 22);
        QColor pillSoft = pill;
        pillSoft.setAlpha(t.bg.lightness() > 128 ? 30 : 50);
        p->setPen(Qt::NoPen);
        p->setBrush(pillSoft);
        p->drawRoundedRect(pillRect, 11, 11);
        p->setPen(pill);
        p->setFont(pillFont);
        p->drawText(pillRect, Qt::AlignCenter, label);

        // name + subtitle
        const int textLeft = icon.right() + 14;
        QFont title = option.font;
        title.setPointSizeF(option.font.pointSizeF() + 0.5);
        title.setBold(true);
        p->setFont(title);
        p->setPen(t.text);
        const QRect titleRect(textLeft, card.top() + 16, right.left() - textLeft - 16, 22);
        p->drawText(titleRect, Qt::AlignLeft | Qt::AlignVCenter,
                    QFontMetrics(title).elidedText(task->name, Qt::ElideMiddle, titleRect.width()));

        QFont sub = option.font;
        sub.setPointSizeF(std::max(8.0, option.font.pointSizeF() - 1.5));
        const QFontMetrics fmSub(sub);
        QStringList parts;
        parts << (task->host.isEmpty() ? QStringLiteral("local") : task->host);
        if (const auto total = task->progress.total.load()) parts << bytes(qint64(total));
        if (task->state == State::Running) parts << QStringLiteral("%1 connections").arg(kConnections);
        if (task->state == State::Failed) parts << task->error;
        p->setFont(sub);
        p->setPen(t.muted);
        const QRect subRect(textLeft, card.top() + 39, right.left() - textLeft - 16, 18);
        p->drawText(subRect, Qt::AlignLeft | Qt::AlignVCenter,
                    fmSub.elidedText(parts.join(QStringLiteral("  ·  ")), Qt::ElideRight, subRect.width()));

        // progress track
        const QRectF track(card.left() + 16, card.bottom() - 19, card.width() - 32 - 96, 8);
        p->setPen(Qt::NoPen);
        p->setBrush(t.border);
        p->drawRoundedRect(track, 4, 4);
        const auto total = task->progress.total.load();
        const auto received = std::min<quint64>(task->progress.received.load(), total ? total : 0);
        double ratio = task->state == State::Done ? 1.0 : (total ? double(received) / double(total) : 0.0);
        ratio = std::clamp(ratio, 0.0, 1.0);
        if (ratio > 0) {
            QColor bar = task->state == State::Done ? t.success
                       : task->state == State::Failed ? t.danger
                       : task->state == State::Paused ? t.warn : t.accent;
            QLinearGradient gradient(track.topLeft(), track.topRight());
            gradient.setColorAt(0.0, bar);
            QColor tail = bar;
            tail.setAlpha(170);
            gradient.setColorAt(1.0, bar.lighter(118));
            p->setBrush(gradient);
            p->drawRoundedRect(QRectF(track.left(), track.top(), std::max(8.0, track.width() * ratio), track.height()), 4, 4);
        }

        // right column: percentage and rate
        p->setFont(sub);
        p->setPen(task->state == State::Failed ? t.danger : t.muted);
        const QRect metaRect(right.left(), card.top() + 43, right.width(), 18);
        QString meta = QStringLiteral("%1%").arg(int(std::lround(ratio * 100)));
        if (task->state == State::Running) meta += QStringLiteral("   ") + speed(task->rate);
        else if (task->state == State::Done) meta = bytes(qint64(total));
        else if (task->state == State::Paused) meta += QStringLiteral("   ") + bytes(qint64(received));
        p->drawText(metaRect, Qt::AlignRight | Qt::AlignVCenter, meta);
        p->restore();
    }

private:
    const Theme* m_theme;
    TaskModel* m_model;
};

// ─────────────────────────────── sidebar delegate ──────────────────────────

class SideDelegate : public QStyledItemDelegate {
public:
    SideDelegate(const Theme* theme, QObject* parent = nullptr) : QStyledItemDelegate(parent), m_theme(theme) {}
    QSize sizeHint(const QStyleOptionViewItem&, const QModelIndex&) const override { return {0, 38}; }
    void paint(QPainter* p, const QStyleOptionViewItem& option, const QModelIndex& index) const override {
        const auto& t = *m_theme;
        p->save();
        p->setRenderHints(QPainter::Antialiasing | QPainter::TextAntialiasing);
        const QRect row = option.rect.adjusted(6, 2, -6, -2);
        const bool selected = option.state & QStyle::State_Selected;
        const bool hover = option.state & QStyle::State_MouseOver;
        if (selected || hover) {
            p->setPen(Qt::NoPen);
            p->setBrush(selected ? t.select : t.hover);
            p->drawRoundedRect(row, 9, 9);
        }
        QFont f = option.font;
        f.setBold(selected);
        p->setFont(f);
        const QColor dot = index.data(Qt::UserRole + 2).value<QColor>();
        if (dot.isValid()) {
            QColor halo = dot;
            halo.setAlpha(selected ? 70 : 45);
            p->setPen(Qt::NoPen);
            p->setBrush(halo);
            p->drawEllipse(QPointF(row.left() + 19, row.center().y()), 7.0, 7.0);
            p->setBrush(dot);
            p->drawEllipse(QPointF(row.left() + 19, row.center().y()), 3.4, 3.4);
        }
        p->setPen(selected ? t.accent : t.text);
        p->drawText(row.adjusted(32, 0, -60, 0), Qt::AlignLeft | Qt::AlignVCenter,
                    index.data(Qt::DisplayRole).toString());
        const auto count = index.data(Qt::UserRole + 1).toInt();
        p->setPen(selected ? t.accent : t.muted);
        p->drawText(row.adjusted(0, 0, -12, 0), Qt::AlignRight | Qt::AlignVCenter,
                    count ? QString::number(count) : QString());
        p->restore();
    }
private:
    const Theme* m_theme;
};

// ─────────────────────────────── add dialog ────────────────────────────────

class AddDialog : public QDialog {
public:
    explicit AddDialog(const Theme& theme, QWidget* parent = nullptr) : QDialog(parent) {
        Q_UNUSED(theme);
        setWindowTitle("New download");
        setMinimumWidth(560);
        auto* layout = new QVBoxLayout(this);
        layout->setContentsMargins(24, 22, 24, 20);
        layout->setSpacing(14);

        auto* heading = new QLabel("New download");
        heading->setObjectName("detailName");
        layout->addWidget(heading);

        auto* urlLabel = new QLabel("URL");
        urlLabel->setObjectName("key");
        m_url = new QLineEdit;
        m_url->setObjectName("field");
        m_url->setPlaceholderText("https://example.com/file.zip");
        layout->addWidget(urlLabel);
        layout->addWidget(m_url);

        auto* destLabel = new QLabel("Save as");
        destLabel->setObjectName("key");
        auto* destRow = new QHBoxLayout;
        m_dest = new QLineEdit;
        m_dest->setObjectName("field");
        m_dest->setPlaceholderText(QDir(defaultFolder()).filePath("file.zip"));
        auto* browse = new QPushButton("Browse…");
        connect(browse, &QPushButton::clicked, this, [this] {
            const auto start = m_dest->text().isEmpty() ? QDir(defaultFolder()).absoluteFilePath("download") : m_dest->text();
            const auto picked = QFileDialog::getSaveFileName(this, "Save download as", start);
            if (!picked.isEmpty()) m_dest->setText(picked);
        });
        destRow->addWidget(m_dest, 1);
        destRow->addWidget(browse);
        layout->addWidget(destLabel);
        layout->addLayout(destRow);

        auto* connRow = new QHBoxLayout;
        auto* connLabel = new QLabel("Connections");
        connLabel->setObjectName("key");
        m_conn = new QSpinBox;
        m_conn->setRange(1, 16);
        m_conn->setValue(int(kConnections));
        connRow->addWidget(connLabel);
        connRow->addStretch();
        connRow->addWidget(m_conn);
        layout->addLayout(connRow);

        auto* hint = new QLabel("Download starts immediately. Pause and resume any time.");
        hint->setObjectName("tagline");
        layout->addWidget(hint);

        auto* buttons = new QHBoxLayout;
        auto* cancel = new QPushButton("Cancel");
        auto* ok = new QPushButton("Download");
        ok->setObjectName("primary");
        ok->setDefault(true);
        buttons->addStretch();
        buttons->addWidget(cancel);
        buttons->addWidget(ok);
        layout->addSpacing(4);
        layout->addLayout(buttons);

        connect(cancel, &QPushButton::clicked, this, &QDialog::reject);
        connect(ok, &QPushButton::clicked, this, &QDialog::accept);
        connect(m_url, &QLineEdit::textChanged, this, [this](const QString& text) {
            if (!m_dest->text().isEmpty() || text.trimmed().isEmpty()) return;
            const QUrl url(text.trimmed(), QUrl::StrictMode);
            if (!url.isValid()) return;
            const auto name = sanitise(url.fileName(QUrl::FullyDecoded));
            m_dest->setPlaceholderText(QDir(defaultFolder()).filePath(name));
        });
        m_url->setFocus();
    }

    QString url() const { return m_url->text().trimmed(); }
    QString destination() const {
        auto text = m_dest->text().trimmed();
        if (text.isEmpty()) {
            const QUrl parsed(url(), QUrl::StrictMode);
            text = QDir(defaultFolder()).filePath(sanitise(parsed.fileName(QUrl::FullyDecoded)));
        }
        return text;
    }
    unsigned connections() const { return unsigned(m_conn->value()); }

    void accept() override {
        const QUrl parsed(url(), QUrl::StrictMode);
        if (!parsed.isValid() || parsed.host().isEmpty() || !parsed.userInfo().isEmpty() ||
            (parsed.scheme() != "http" && parsed.scheme() != "https")) {
            QMessageBox::warning(this, "Invalid URL", "Enter an HTTP or HTTPS URL without embedded credentials.");
            return;
        }
        if (destination().isEmpty()) {
            QMessageBox::warning(this, "Missing destination", "Choose where the file should be saved.");
            return;
        }
        QDialog::accept();
    }

private:
    QLineEdit* m_url = nullptr;
    QLineEdit* m_dest = nullptr;
    QSpinBox* m_conn = nullptr;
};

// ─────────────────────────────── main window ───────────────────────────────

class Window : public QMainWindow {
public:
    explicit Window(bool dark) : m_theme(dark ? Theme::dark() : Theme::light()) {
        m_dark = dark;
        setWindowTitle("ODM — Open Download Manager");
        setMinimumSize(900, 580);
        resize(1180, 720);
        build();
        applyTheme();
        m_timer.start(200);
        connect(&m_timer, &QTimer::timeout, this, [this] { tick(); });
    }

    void enqueue(const QString& url, const QString& destination, unsigned connections) {
        Q_UNUSED(connections);
        const QUrl parsed(url, QUrl::StrictMode);
        auto task = std::make_unique<Task>();
        task->url = QString::fromLatin1(parsed.toEncoded(QUrl::RemoveFragment));
        task->path = destination;
        task->host = parsed.host();
        task->name = QFileInfo(destination).fileName();
        const int row = m_model->append(std::move(task));
        m_list->setCurrentIndex(m_proxy->mapFromSource(m_model->index(row)));
        startTask(*m_model->at(row));
        refresh();
    }

protected:
    void closeEvent(QCloseEvent* event) override {
        bool busy = false;
        for (auto& task : m_model->tasks) {
            if (!task->worker.valid()) continue;
            task->progress.pause = true;
            busy = true;
        }
        if (busy) {
            m_closing = true;
            m_footerLeft->setText("Finishing up — pausing downloads…");
            event->ignore();
            return;
        }
        event->accept();
    }

private:
    void build() {
        auto* root = new QWidget;
        root->setObjectName("root");
        auto* outer = new QVBoxLayout(root);
        outer->setContentsMargins(0, 0, 0, 0);
        outer->setSpacing(0);
        outer->addWidget(buildStrip());
        outer->addWidget(buildHeader());

        auto* body = new QHBoxLayout;
        body->setContentsMargins(16, 16, 16, 12);
        body->setSpacing(16);
        body->addWidget(buildSidebar());
        body->addWidget(buildList(), 1);
        body->addWidget(buildDetail());
        outer->addLayout(body, 1);
        outer->addWidget(buildFooter());
        setCentralWidget(root);
    }

    QWidget* buildStrip() {
        auto* strip = new QFrame;
        strip->setObjectName("strip");
        strip->setFixedHeight(3);
        return strip;
    }

    QWidget* buildHeader() {
        auto* header = new QFrame;
        header->setObjectName("header");
        header->setFixedHeight(66);
        auto* layout = new QHBoxLayout(header);
        layout->setContentsMargins(20, 0, 20, 0);
        layout->setSpacing(12);

        auto* logo = new QLabel("ODM");
        logo->setObjectName("logo");
        logo->setFixedSize(38, 38);
        logo->setAlignment(Qt::AlignCenter);
        layout->addWidget(logo);

        auto* titles = new QVBoxLayout;
        titles->setSpacing(0);
        auto* wordmark = new QLabel("Open Download Manager");
        wordmark->setObjectName("wordmark");
        auto* tagline = new QLabel("Multi-connection HTTP / HTTPS transfers");
        tagline->setObjectName("tagline");
        titles->addWidget(wordmark);
        titles->addWidget(tagline);
        layout->addLayout(titles);
        layout->addStretch();

        m_search = new QLineEdit;
        m_search->setObjectName("search");
        m_search->setPlaceholderText("Search downloads");
        m_search->setClearButtonEnabled(true);
        m_search->setFixedWidth(240);
        connect(m_search, &QLineEdit::textChanged, this, [this](const QString& text) {
            m_proxy->query = text.trimmed();
            m_proxy->applyFilter();
            refresh();
        });
        layout->addWidget(m_search);

        auto* themeButton = new QPushButton(m_dark ? "☀  Light" : "☾  Dark");
        themeButton->setObjectName("ghost");
        themeButton->setCursor(Qt::PointingHandCursor);
        connect(themeButton, &QPushButton::clicked, this, [this, themeButton] {
            m_dark = !m_dark;
            m_theme = m_dark ? Theme::dark() : Theme::light();
            themeButton->setText(m_dark ? "☀  Light" : "☾  Dark");
            applyTheme();
        });
        m_themeButton = themeButton;
        layout->addWidget(themeButton);

        auto* add = new QPushButton("＋  New download");
        add->setObjectName("primary");
        add->setCursor(Qt::PointingHandCursor);
        add->setShortcut(QKeySequence::New);
        connect(add, &QPushButton::clicked, this, [this] { addDialog(); });
        layout->addWidget(add);
        return header;
    }

    QWidget* buildSidebar() {
        m_side = new QListView;
        m_side->setObjectName("side");
        m_side->setFixedWidth(196);
        m_side->setSelectionMode(QAbstractItemView::SingleSelection);
        m_side->setEditTriggers(QAbstractItemView::NoEditTriggers);
        m_side->setVerticalScrollMode(QAbstractItemView::ScrollPerPixel);
        m_side->setFocusPolicy(Qt::NoFocus);
        m_side->setMouseTracking(true);
        m_side->viewport()->setMouseTracking(true);
        m_side->setItemDelegate(new SideDelegate(&m_theme, this));
        auto* model = new QStandardItemModel(this);
        const QStringList labels{"All downloads", "Downloading", "Queued", "Completed", "Paused / failed"};
        const QColor dots[] = {m_theme.accent, m_theme.accent, m_theme.muted, m_theme.success, m_theme.warn};
        int row = 0;
        for (const auto& label : labels) {
            auto* item = new QStandardItem(label);
            item->setData(0, Qt::UserRole + 1);
            item->setData(dots[row++], Qt::UserRole + 2);
            model->appendRow(item);
        }
        m_side->setModel(model);
        m_side->setCurrentIndex(model->index(0, 0));
        connect(m_side->selectionModel(), &QItemSelectionModel::currentChanged, this, [this](const QModelIndex& now) {
            if (!now.isValid()) return;
            m_proxy->filter = now.row();
            m_proxy->applyFilter();
            refresh();
        });

        auto* panel = new QFrame;
        panel->setObjectName("panel");
        panel->setFixedWidth(212);
        auto* layout = new QVBoxLayout(panel);
        layout->setContentsMargins(10, 14, 10, 12);
        layout->setSpacing(8);
        auto* heading = new QLabel("LIBRARY");
        heading->setObjectName("sectionTitle");
        heading->setContentsMargins(10, 0, 0, 0);
        layout->addWidget(heading);
        layout->addWidget(m_side);
        layout->addStretch();
        auto* hint = new QLabel("HTTP / HTTPS\nResume supported");
        hint->setObjectName("tagline");
        hint->setContentsMargins(10, 0, 0, 0);
        layout->addWidget(hint);
        return panel;
    }

    QWidget* buildList() {
        m_model = new TaskModel(this);
        m_proxy = new FilterProxy(m_model, this);
        m_list = new QListView;
        m_list->setObjectName("list");
        m_list->setModel(m_proxy);
        m_list->setItemDelegate(new TaskDelegate(&m_theme, m_model, this));
        m_list->setSelectionMode(QAbstractItemView::SingleSelection);
        m_list->setEditTriggers(QAbstractItemView::NoEditTriggers);
        m_list->setVerticalScrollMode(QAbstractItemView::ScrollPerPixel);
        m_list->setMouseTracking(true);
        m_list->viewport()->setMouseTracking(true);
        m_list->setContextMenuPolicy(Qt::CustomContextMenu);
        m_list->setSpacing(0);
        connect(m_list->selectionModel(), &QItemSelectionModel::currentChanged, this, [this] { refreshDetail(); });
        connect(m_list, &QListView::customContextMenuRequested, this, [this](const QPoint& pos) { contextMenu(pos); });
        connect(m_list, &QListView::doubleClicked, this, [this] { openSelected(); });

        auto* empty = new QWidget;
        auto* emptyLayout = new QVBoxLayout(empty);
        emptyLayout->setAlignment(Qt::AlignCenter);
        emptyLayout->setSpacing(10);
        auto* icon = new QLabel("⬇");
        icon->setObjectName("logo");
        icon->setAlignment(Qt::AlignCenter);
        icon->setFixedSize(56, 56);
        auto* title = new QLabel("Nothing here yet");
        title->setObjectName("emptyTitle");
        title->setAlignment(Qt::AlignCenter);
        auto* text = new QLabel("Add a URL to start a multi-connection download.");
        text->setObjectName("empty");
        text->setAlignment(Qt::AlignCenter);
        auto* button = new QPushButton("＋  New download");
        button->setObjectName("primary");
        button->setCursor(Qt::PointingHandCursor);
        connect(button, &QPushButton::clicked, this, [this] { addDialog(); });
        auto* row = new QHBoxLayout;
        row->addStretch();
        row->addWidget(button);
        row->addStretch();
        emptyLayout->addWidget(icon, 0, Qt::AlignHCenter);
        emptyLayout->addWidget(title);
        emptyLayout->addWidget(text);
        emptyLayout->addLayout(row);
        m_empty = empty;

        auto* container = new QWidget;
        auto* stack = new QVBoxLayout(container);
        stack->setContentsMargins(4, 2, 4, 0);
        stack->setSpacing(10);
        auto* bar = new QHBoxLayout;
        bar->setSpacing(10);
        auto* barTitle = new QLabel("DOWNLOADS");
        barTitle->setObjectName("sectionTitle");
        m_listCount = new QLabel("0 files");
        m_listCount->setObjectName("detailHost");
        auto* clear = new QPushButton("Clear completed");
        clear->setObjectName("ghost");
        clear->setCursor(Qt::PointingHandCursor);
        connect(clear, &QPushButton::clicked, this, [this] { clearCompleted(); });
        bar->addWidget(title);
        bar->addWidget(m_listCount);
        bar->addStretch();
        bar->addWidget(clear);
        stack->addLayout(bar);
        stack->addWidget(m_list, 1);
        stack->addWidget(m_empty, 1);
        m_empty->hide();
        return container;
    }

    QWidget* buildDetail() {
        auto* panel = new QFrame;
        panel->setObjectName("detail");
        panel->setFixedWidth(300);
        auto* layout = new QVBoxLayout(panel);
        layout->setContentsMargins(18, 18, 18, 16);
        layout->setSpacing(6);

        m_detailBadge = new QLabel("FILE");
        m_detailBadge->setObjectName("logo");
        m_detailBadge->setFixedSize(44, 44);
        m_detailBadge->setAlignment(Qt::AlignCenter);
        m_detailName = new QLabel("No download selected");
        m_detailName->setObjectName("detailName");
        m_detailName->setWordWrap(true);
        m_detailHost = new QLabel("Select a row to see details");
        m_detailHost->setObjectName("detailHost");
        m_detailHost->setWordWrap(true);

        auto* head = new QHBoxLayout;
        head->setSpacing(12);
        head->addWidget(m_detailBadge, 0, Qt::AlignTop);
        auto* headText = new QVBoxLayout;
        headText->setSpacing(2);
        headText->addWidget(m_detailName);
        headText->addWidget(m_detailHost);
        head->addLayout(headText, 1);
        layout->addLayout(head);
        layout->addSpacing(10);

        struct Row { const char* key; QLabel** target; };
        const Row rows[] = {{"Status", &m_detailStatus}, {"Size", &m_detailSize}, {"Downloaded", &m_detailDone},
                            {"Speed", &m_detailSpeed}, {"Connections", &m_detailConn}, {"Saved to", &m_detailPath},
                            {"URL", &m_detailUrl}};
        for (const auto& row : rows) {
            auto* key = new QLabel(row.key);
            key->setObjectName("key");
            auto* value = new QLabel("—");
            value->setObjectName("value");
            value->setWordWrap(true);
            value->setTextInteractionFlags(Qt::TextSelectableByMouse);
            *row.target = value;
            layout->addWidget(key);
            layout->addWidget(value);
            layout->addSpacing(4);
        }
        layout->addStretch();

        auto* divider = new QFrame;
        divider->setObjectName("divider");
        divider->setFixedHeight(1);
        layout->addWidget(divider);
        layout->addSpacing(4);

        auto* buttons = new QHBoxLayout;
        buttons->setSpacing(8);
        m_open = new QPushButton("Open");
        m_reveal = new QPushButton("Folder");
        m_cancel = new QPushButton("Pause");
        for (auto* b : {m_open, m_reveal, m_cancel}) b->setCursor(Qt::PointingHandCursor);
        connect(m_open, &QPushButton::clicked, this, [this] { openSelected(); });
        connect(m_reveal, &QPushButton::clicked, this, [this] { revealSelected(); });
        connect(m_cancel, &QPushButton::clicked, this, [this] { togglePause(); });
        buttons->addWidget(m_open);
        buttons->addWidget(m_reveal);
        buttons->addWidget(m_cancel);
        layout->addLayout(buttons);
        return panel;
    }

    QWidget* buildFooter() {
        auto* footer = new QFrame;
        footer->setObjectName("footer");
        footer->setFixedHeight(38);
        auto* layout = new QHBoxLayout(footer);
        layout->setContentsMargins(20, 0, 20, 0);
        m_footerLeft = new QLabel("Ready");
        m_footerLeft->setObjectName("footer");
        m_footerRight = new QLabel(QStringLiteral("Up to %1 connections").arg(kConnections));
        m_footerRight->setObjectName("footer");
        layout->addWidget(m_footerLeft);
        layout->addStretch();
        layout->addWidget(m_footerRight);
        return footer;
    }

    void applyTheme() {
        qApp->setStyleSheet(themeStyleSheet(m_theme));
#ifdef Q_OS_WIN
        if (HWND hwnd = reinterpret_cast<HWND>(winId())) {
            using Pfn = HRESULT(WINAPI*)(HWND, DWORD, LPCVOID, DWORD);
            if (HMODULE dwm = LoadLibraryW(L"dwmapi.dll")) {
                if (auto fn = reinterpret_cast<Pfn>(GetProcAddress(dwm, "DwmSetWindowAttribute"))) {
                    const BOOL value = m_dark ? TRUE : FALSE;
                    if (FAILED(fn(hwnd, 20, &value, sizeof(value)))) fn(hwnd, 19, &value, sizeof(value));
                }
            }
        }
#endif
        if (m_detailBadge) {
            m_detailBadge->setStyleSheet(QStringLiteral("background: %1; color: #FFFFFF; border-radius: 12px;"
                                                        "font-size: 11px; font-weight: 700;")
                                             .arg(m_theme.accent.name()));
        }
        m_list->viewport()->update();
        m_side->viewport()->update();
    }

    Task* current() {
        const auto index = m_list->currentIndex();
        if (!index.isValid()) return nullptr;
        return m_model->at(m_proxy->mapToSource(index).row());
    }

    void startTask(Task& task) {
        if (task.worker.valid() || task.state == State::Running || task.state == State::Done) return;
        task.progress.pause = false;
        task.pausedByUser = false;
        task.state = State::Running;
        task.error.clear();
        task.lastBytes = qint64(task.progress.received.load());
        task.sample.start();
        Task* target = &task;
        const auto connections = m_connections;
        try {
            task.worker = std::async(std::launch::async, [target, connections] {
                return odm::download(target->url.toStdWString(), target->path.toStdWString(), connections,
                                     target->progress);
            });
        } catch (const std::exception& e) {
            task.state = State::Failed;
            task.error = QString::fromUtf8(e.what());
        }
    }

    void tick() {
        bool busy = false;
        for (int row = 0; row < m_model->rowCount(); ++row) {
            auto& task = *m_model->at(row);
            if (task.worker.valid()) {
                busy = true;
                if (task.worker.wait_for(std::chrono::seconds(0)) == std::future_status::ready) {
                    bool ok = false;
                    try {
                        ok = task.worker.get();
                    } catch (const std::exception& e) {
                        task.error = QString::fromUtf8(e.what());
                    }
                    task.state = ok ? State::Done : (task.error.isEmpty() ? State::Paused : State::Failed);
                    task.rate = 0;
                } else if (task.sample.elapsed() >= 400) {
                    const auto now = qint64(task.progress.received.load());
                    const double instant = double(now - task.lastBytes) * 1000.0 / double(task.sample.elapsed());
                    task.rate = task.rate > 0 ? 0.65 * task.rate + 0.35 * std::max(0.0, instant) : std::max(0.0, instant);
                    task.lastBytes = now;
                    task.sample.restart();
                }
            }
            m_model->notify(row);
        }
        if (m_closing && !busy) {
            m_closing = false;
            close();
            return;
        }
        refresh();
    }

    void refresh() {
        int counts[5] = {0, 0, 0, 0, 0};
        int active = 0;
        double totalRate = 0;
        for (const auto& task : m_model->tasks) {
            counts[0]++;
            switch (task->state) {
                case State::Running: counts[1]++; active++; break;
                case State::Queued: counts[2]++; break;
                case State::Done: counts[3]++; break;
                default: counts[4]++; break;
            }
            totalRate += task->rate;
        }
        auto* model = qobject_cast<QStandardItemModel*>(m_side->model());
        for (int i = 0; i < 5 && model && i < model->rowCount(); ++i) {
            model->item(i)->setData(counts[i], Qt::UserRole + 1);
        }
        m_side->viewport()->update();

        const bool empty = m_proxy->rowCount() == 0;
        m_list->setVisible(!empty);
        m_empty->setVisible(empty);
        m_listCount->setText(m_proxy->rowCount() == 1 ? QStringLiteral("1 file")
                                                      : QStringLiteral("%1 files").arg(m_proxy->rowCount()));

        QString left = QStringLiteral("%1 download%2").arg(counts[0]).arg(counts[0] == 1 ? "" : "s");
        if (active) left += QStringLiteral("  ·  %1 active").arg(active);
        m_footerLeft->setText(left);
        m_footerRight->setText(totalRate > 0 ? speed(totalRate) + QStringLiteral("  ·  up to %1 connections").arg(m_connections)
                                             : QStringLiteral("Up to %1 connections").arg(m_connections));
        refreshDetail();
    }

    void refreshDetail() {
        auto* task = current();
        if (!task) {
            m_detailBadge->setText("—");
            m_detailName->setText("No download selected");
            m_detailHost->setText("Select a row to see details");
            for (auto* label : {m_detailStatus, m_detailSize, m_detailDone, m_detailSpeed, m_detailConn,
                                m_detailPath, m_detailUrl})
                label->setText("—");
            m_open->setEnabled(false);
            m_reveal->setEnabled(false);
            m_cancel->setEnabled(false);
            return;
        }
        const auto total = qint64(task->progress.total.load());
        const auto received = qint64(task->progress.received.load());
        m_detailBadge->setText(badge(task->name));
        m_detailBadge->setStyleSheet(QStringLiteral("background: %1; color: #FFFFFF; border-radius: 12px;"
                                                    "font-size: 11px; font-weight: 700;")
                                         .arg(badgeColor(task->name, m_theme).name()));
        m_detailName->setText(task->name);
        m_detailHost->setText(task->host.isEmpty() ? QStringLiteral("local") : task->host);
        const char* states[] = {"Queued", "Downloading", "Paused", "Complete", "Failed"};
        m_detailStatus->setText(task->state == State::Failed && !task->error.isEmpty()
                                    ? QStringLiteral("Failed — %1").arg(task->error)
                                    : QString::fromLatin1(states[int(task->state)]));
        m_detailStatus->setStyleSheet(QStringLiteral("color: %1; font-size: 12px;")
                                          .arg(task->state == State::Failed ? m_theme.danger.name()
                                               : task->state == State::Done ? m_theme.success.name()
                                               : m_theme.text.name()));
        m_detailSize->setText(bytes(total));
        m_detailDone->setText(bytes(received) + (total ? QStringLiteral("  (%1%)").arg(int(100.0 * std::min<qint64>(received, total) / total)) : QString()));
        m_detailSpeed->setText(task->state == State::Running ? speed(task->rate) : QStringLiteral("—"));
        m_detailConn->setText(task->state == State::Running ? QString::number(m_connections) : QStringLiteral("—"));
        m_detailPath->setText(QDir::toNativeSeparators(task->path));
        m_detailUrl->setText(task->url);
        m_open->setEnabled(task->state == State::Done);
        m_reveal->setEnabled(task->state == State::Done);
        m_cancel->setEnabled(true);
        m_cancel->setText(task->state == State::Running ? "Pause" : task->state == State::Done ? "Pause" : "Resume");
        m_cancel->setEnabled(task->state == State::Running || task->state == State::Paused ||
                             task->state == State::Failed || task->state == State::Queued);
    }

    void togglePause() {
        auto* task = current();
        if (!task) return;
        if (task->state == State::Running) {
            task->progress.pause = true;
            task->pausedByUser = true;
        } else if (task->state != State::Done) {
            startTask(*task);
        }
        refresh();
    }

    void clearCompleted() {
        for (int row = m_model->rowCount() - 1; row >= 0; --row) {
            const auto& task = *m_model->at(row);
            if (task.state == State::Done && !task.worker.valid()) m_model->erase(row);
        }
        refresh();
    }

    void openSelected() {
        auto* task = current();
        if (task && task->state == State::Done) QDesktopServices::openUrl(QUrl::fromLocalFile(task->path));
    }

    void revealSelected() {
        auto* task = current();
        if (!task) return;
        QProcess::startDetached("explorer.exe", {QStringLiteral("/select,") + QDir::toNativeSeparators(task->path)});
    }

    void removeSelected() {
        auto* task = current();
        if (!task || task->worker.valid()) return;
        const int sourceRow = m_proxy->mapToSource(m_list->currentIndex()).row();
        m_model->erase(sourceRow);
        refresh();
    }

    void contextMenu(const QPoint& pos) {
        const auto index = m_list->indexAt(pos);
        if (!index.isValid()) return;
        m_list->setCurrentIndex(index);
        auto* task = current();
        if (!task) return;
        QMenu menu(this);
        menu.addAction("Open", this, [this] { openSelected(); })->setEnabled(task->state == State::Done);
        menu.addAction("Show in folder", this, [this] { revealSelected(); })->setEnabled(task->state == State::Done);
        menu.addSeparator();
        menu.addAction(task->state == State::Running ? "Pause" : "Resume", this, [this] { togglePause(); })
            ->setEnabled(task->state != State::Done);
        menu.addAction("Copy URL", this, [task] { QGuiApplication::clipboard()->setText(task->url); });
        menu.addSeparator();
        menu.addAction("Remove from list", this, [this] { removeSelected(); })->setEnabled(!task->worker.valid());
        menu.exec(m_list->viewport()->mapToGlobal(pos));
    }

    void addDialog() {
        AddDialog dialog(m_theme, this);
        if (dialog.exec() != QDialog::Accepted) return;
        const auto dest = dialog.destination();
        for (const auto& task : m_model->tasks) {
            if (task->path.compare(QDir::cleanPath(dest), Qt::CaseInsensitive) == 0) {
                QMessageBox::information(this, "Already in the list",
                                         "That destination is already in the list. Use Resume to retry it.");
                return;
            }
        }
        m_connections = dialog.connections();
        enqueue(dialog.url(), dest, dialog.connections());
    }

    Theme m_theme;
    bool m_dark = false;
    bool m_closing = false;
    unsigned m_connections = kConnections;
    QTimer m_timer;

    TaskModel* m_model = nullptr;
    FilterProxy* m_proxy = nullptr;
    QListView* m_list = nullptr;
    QListView* m_side = nullptr;
    QWidget* m_empty = nullptr;
    QLineEdit* m_search = nullptr;
    QLabel* m_listCount = nullptr;
    QLabel* m_footerLeft = nullptr;
    QLabel* m_footerRight = nullptr;
    QPushButton* m_themeButton = nullptr;

    QLabel* m_detailBadge = nullptr;
    QLabel* m_detailName = nullptr;
    QLabel* m_detailHost = nullptr;
    QLabel* m_detailStatus = nullptr;
    QLabel* m_detailSize = nullptr;
    QLabel* m_detailDone = nullptr;
    QLabel* m_detailSpeed = nullptr;
    QLabel* m_detailConn = nullptr;
    QLabel* m_detailPath = nullptr;
    QLabel* m_detailUrl = nullptr;
    QPushButton* m_open = nullptr;
    QPushButton* m_reveal = nullptr;
    QPushButton* m_cancel = nullptr;
};

int main(int argc, char** argv) {
    QApplication app(argc, argv);
    QApplication::setFont(QFont("Segoe UI", 10));
    app.setApplicationName("ODM");
    app.setApplicationDisplayName("Open Download Manager");

    QStringList urls;
    QString output;
    QString shot;
    bool dark = false;
    for (int i = 1; i < argc; ++i) {
        const QString arg = QString::fromLocal8Bit(argv[i]);
        if (arg == "--dark") {
            dark = true;
        } else if (arg == "--screenshot" && i + 1 < argc) {
            shot = QString::fromLocal8Bit(argv[++i]);
        } else if ((arg == "-o" || arg == "--output") && i + 1 < argc) {
            output = QString::fromLocal8Bit(argv[++i]);
        } else if (!arg.startsWith('-')) {
            urls << arg;
        }
    }

    Window window(dark);
    window.show();
    for (const auto& url : urls) {
        const QUrl parsed(url, QUrl::StrictMode);
        auto destination = output;
        if (destination.isEmpty()) destination = QDir(defaultFolder()).filePath(sanitise(parsed.fileName(QUrl::FullyDecoded)));
        window.enqueue(url, destination, kConnections);
    }
    if (!shot.isEmpty()) {
        QTimer::singleShot(2500, &window, [&window, shot] {
            window.grab().save(shot);
            QCoreApplication::quit();
        });
    }
    return app.exec();
}
