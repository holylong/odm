#include "core.h"
#include <QtWidgets>
#include <QStandardPaths>
#include <QUrl>
#include <future>
#include <memory>
#include <vector>

class Window : public QMainWindow {
    struct Task {
        QString url, path, status = "Queued";
        odm::Progress progress;
        std::future<bool> worker;
        QProgressBar* bar = nullptr;
    };
    std::vector<std::unique_ptr<Task>> tasks;
    QTableWidget* table = new QTableWidget(0, 4);
    bool closing = false;

    void start(Task& task) {
        if (task.worker.valid() || task.status == "Complete") return;
        task.progress.pause = false;
        task.status = "Downloading";
        try {
            task.worker = std::async(std::launch::async, [&task] {
                return odm::download(task.url.toStdWString(), task.path.toStdWString(), 8, task.progress);
            });
        } catch (const std::exception& e) { task.status = QString::fromUtf8(e.what()); }
    }
    Task* selected() {
        const auto row = table->currentRow();
        return row >= 0 ? tasks.at(row).get() : nullptr;
    }
    void tick() {
        bool busy = false;
        for (int row = 0; row < table->rowCount(); ++row) {
            auto& t = *tasks[row];
            if (t.worker.valid() && t.worker.wait_for(std::chrono::seconds(0)) == std::future_status::ready) {
                try { t.status = t.worker.get() ? "Complete" : "Paused"; }
                catch (const std::exception& e) { t.status = QString::fromUtf8(e.what()); }
            }
            busy |= t.worker.valid();
            const auto size = t.progress.total.load(), bytes = t.progress.received.load();
            t.bar->setRange(0, !size && t.worker.valid() ? 0 : 100);
            t.bar->setValue(t.status == "Complete" ? 100 : size ? int(qMin(100.0, 100.0 * bytes / size)) : 0);
            table->item(row, 2)->setText(t.status);
            table->item(row, 3)->setText(QLocale().formattedDataSize(qint64(bytes)));
        }
        if (closing && !busy) close();
    }
    void add() {
        const auto url = QUrl(QInputDialog::getText(this, "New download", "HTTP or HTTPS URL:").trimmed(), QUrl::StrictMode);
        if (url.isEmpty()) return;
        if (!url.isValid() || url.host().isEmpty() || !url.userInfo().isEmpty() ||
            (url.scheme() != "https" && url.scheme() != "http")) {
            QMessageBox::warning(this, "Invalid URL", "Enter an HTTP or HTTPS URL without credentials.");
            return;
        }
        auto name = url.fileName(QUrl::FullyDecoded);
        name.replace(QRegularExpression(QStringLiteral("[<>:\"/\\\\|?*\\x00-\\x1f]")), "_");
        if (name.isEmpty() || name == "." || name == "..") name = "download";
        const auto dir = QStandardPaths::writableLocation(QStandardPaths::DownloadLocation);
        const auto path = QFileDialog::getSaveFileName(this, "Save download as", QDir(dir).filePath(name));
        if (path.isEmpty()) return;
        for (const auto& t : tasks) if (t->path.compare(path, Qt::CaseInsensitive) == 0) {
            QMessageBox::warning(this, "Duplicate destination", "Use Resume to retry an existing task, or select another file.");
            return;
        }
        auto t = std::make_unique<Task>();
        t->url = QString::fromLatin1(url.toEncoded(QUrl::RemoveFragment));
        t->path = path;
        const int row = table->rowCount();
        table->insertRow(row);
        auto* file = new QTableWidgetItem(QFileInfo(path).fileName());
        file->setToolTip(path);
        table->setItem(row, 0, file);
        t->bar = new QProgressBar;
        table->setCellWidget(row, 1, t->bar);
        table->setItem(row, 2, new QTableWidgetItem(t->status));
        table->setItem(row, 3, new QTableWidgetItem);
        tasks.push_back(std::move(t));
        table->selectRow(row);
        start(*tasks.back());
    }
public:
    Window() {
        setWindowTitle("ODM — Open Download Manager");
        resize(960, 540);
        table->setHorizontalHeaderLabels({"File", "Progress", "Status", "Downloaded"});
        table->setSelectionBehavior(QAbstractItemView::SelectRows);
        table->setSelectionMode(QAbstractItemView::SingleSelection);
        table->setEditTriggers(QAbstractItemView::NoEditTriggers);
        table->setAlternatingRowColors(true);
        table->verticalHeader()->hide();
        table->horizontalHeader()->setSectionResizeMode(0, QHeaderView::Stretch);
        table->horizontalHeader()->setSectionResizeMode(1, QHeaderView::Stretch);
        table->horizontalHeader()->setSectionResizeMode(2, QHeaderView::Stretch);
        table->setColumnWidth(3, 125);
        setCentralWidget(table);
        auto* toolbar = addToolBar("Downloads");
        toolbar->setMovable(false);
        toolbar->addAction("Add URL", QKeySequence(QKeySequence::New), this, [this] { if (!closing) add(); });
        toolbar->addAction("Pause", this, [this] { if (auto* t = selected(); t && t->worker.valid()) {
            t->progress.pause = true;
            t->status = "Pausing…";
        }});
        toolbar->addAction("Resume / Retry", this, [this] { if (!closing) if (auto* t = selected()) start(*t); });
        statusBar()->showMessage("HTTP / HTTPS downloads  •  Ctrl+N to add a URL");
        auto* timer = new QTimer(this);
        connect(timer, &QTimer::timeout, this, [this] { tick(); });
        timer->start(150);
    }
    void closeEvent(QCloseEvent* event) override {
        for (auto& t : tasks) if (t->worker.valid()) {
            closing = true;
            t->progress.pause = true;
        }
        if (closing) for (auto& t : tasks) if (t->worker.valid()) {
            statusBar()->showMessage("Pausing downloads before exit…");
            event->ignore();
            return;
        }
        event->accept();
    }
};

int main(int argc, char** argv) {
    QApplication app(argc, argv);
    Window window;
    window.show();
    return app.exec();
}
